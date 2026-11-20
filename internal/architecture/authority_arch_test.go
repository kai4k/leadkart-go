// authority_arch_test.go — ADR 0069: every identity command that writes
// authority (role assignments, permission overrides, membership status, manager
// links, roles and their permissions, permission-request decisions) goes
// through the authority writer, i.e. runs inside authorityWriter.locked: one
// transaction, the tenant authority lock, the fresh acting member, the policy.
package architecture_test

import (
	"go/ast"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// authorityWriterEntries are the only calls whose function-literal argument is
// "inside the authority writer": locked is the writer, updateMember and
// updateRole are its two wrappers (both end in locked).
var authorityWriterEntries = []string{"locked", "updateMember", "updateRole"}

// authorityWriteMethods are the repository calls that persist authority state.
// The receiver (the last path element) names the aggregate store: memberships
// (role assignments, overrides, status, manager link), roles (the role and its
// permissions) and requests (a permission-request decision).
var authorityWriteMethods = map[string][]string{
	"UpdateByID": {"memberships", "roles", "requests"},
	"Add":        {"roles"}, // creating a role; new memberships and requests carry no authority
}

// authorityWriteExceptions pins the writes outside the writer. The list is
// asserted EXACTLY: an entry that no longer matches a write fails the test, so
// a new bypass cannot hide behind an old reason. Key: "<file>:<receiver>.<func>".
var authorityWriteExceptions = map[string]string{
	"register_tenant.go:RegisterTenantHandler.seedRolesAndAssignOwner": "the first Company Owner of a brand-new tenant: no other member, no actor, nothing to decide",
}

// authorityWriteOffences returns "<file>:<line> <what>" for every authority
// write in src that is not lexically inside a function literal passed to an
// authority writer entry, and the exception keys it used. A file is the
// command package's non-test source.
func authorityWriteOffences(file string, src []byte, t *testing.T) (offences, usedExceptions []string) {
	t.Helper()
	fset, f := parseFile(t, file, src)
	base := filepath.Base(file)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		owner := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) == 1 {
			owner = recvTypeName(fn.Recv.List[0].Type) + "." + owner
		}
		var walk func(n ast.Node, inside bool)
		walk = func(n ast.Node, inside bool) {
			ast.Inspect(n, func(c ast.Node) bool {
				call, ok := c.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := callName(call.Fun)
				// descend into the arguments, marking function literals handed to a writer entry
				entry := slices.Contains(authorityWriterEntries, name)
				walk(call.Fun, inside)
				for _, a := range call.Args {
					if lit, ok := a.(*ast.FuncLit); ok {
						walk(lit.Body, inside || entry)
					} else {
						walk(a, inside)
					}
				}
				if recv, isWrite := authorityWrite(call); isWrite && !inside {
					key := base + ":" + owner
					if _, allowed := authorityWriteExceptions[key]; allowed {
						usedExceptions = append(usedExceptions, key)
					} else {
						offences = append(offences, base+":"+strconv.Itoa(fset.Position(call.Pos()).Line)+" "+recv+"."+name+" in "+owner)
					}
				}
				return false // arguments handled above; the callee expression holds no further calls we care about
			})
		}
		walk(fn.Body, false)
	}
	return offences, usedExceptions
}

// authorityWrite reports whether call persists authority state: a method of
// authorityWriteMethods whose receiver path ends in one of its store names.
func authorityWrite(call *ast.CallExpr) (recv string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", false
	}
	stores, known := authorityWriteMethods[sel.Sel.Name]
	if !known {
		return "", false
	}
	recv = exprText(sel.X)
	last := recv[strings.LastIndex(recv, ".")+1:]
	return recv, slices.Contains(stores, last)
}

func recvTypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvTypeName(x.X)
	}
	return "?"
}

// TestArch_AuthorityWritesGoThroughTheAuthorityWriter fails when a command in
// internal/identity/app/command writes role assignments, permission overrides,
// membership status, manager links, roles or permission-request decisions
// (memberships / roles / requests UpdateByID, roles Add) outside a function
// literal handed to the authority writer (locked, updateMember, updateRole),
// when the exceptions list is stale, or when anything outside the command
// package (the HTTP ports, the queries) calls those repository writes at all.
//
// Scope: production — non-test files of internal/identity/app/command, ports and app/query.
//
// arch-test:no-negative-fixture (RED proof: TestAuthorityWriteScanner_FlagsABypass feeds the scanner a command that writes outside the writer; and bypassing the writer in one real command, recorded in the ADR 0069 commit)
func TestArch_AuthorityWritesGoThroughTheAuthorityWriter(t *testing.T) {
	t.Parallel()
	identity := filepath.Join(internalDir(t), "identity")
	var used []string
	walkGoFiles(t, filepath.Join(identity, "app", "command"), false, func(path string, src []byte) {
		off, ex := authorityWriteOffences(path, src, t)
		used = append(used, ex...)
		for _, o := range off {
			t.Errorf("%s — an authority write outside authorityWriter.locked/updateMember/updateRole: it skips the tenant lock, the fresh actor and the policy (ADR 0069)", o)
		}
	})
	for key := range authorityWriteExceptions {
		if !slices.Contains(used, key) {
			t.Errorf("authorityWriteExceptions entry %q matches no write — remove it", key)
		}
	}
	for _, dir := range []string{filepath.Join(identity, "ports"), filepath.Join(identity, "app", "query")} {
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if recv, isWrite := authorityWrite(call); isWrite {
						t.Errorf("%s:%d %s.%s — authority is written by commands only (ADR 0069)", pathToSlash(path), fset.Position(call.Pos()).Line, recv, callName(call.Fun))
					}
				}
				return true
			})
		})
	}
}

// TestAuthorityWriteScanner_FlagsABypass pins what the scanner flags and what
// it lets through, on synthetic command source.
func TestAuthorityWriteScanner_FlagsABypass(t *testing.T) {
	t.Parallel()
	const bypass = `package command
type H struct{ w authorityWriter }
func (h H) Handle() error {
	return h.w.facts.memberships.UpdateByID(nil, "t", "m", func(m *M) (bool, error) { return true, m.AssignRole("r", now) })
}`
	const viaWriter = `package command
type H struct{ w authorityWriter }
func (h H) Handle() error {
	return h.w.locked(nil, "t", a, now, func(ctx C, actor A) error {
		return h.w.facts.memberships.UpdateByID(ctx, "t", "m", func(m *M) (bool, error) { return true, nil })
	})
}`
	const viaUpdateRole = `package command
func (h H) Handle() error {
	return h.updateRole(nil, "t", "r", a, now, func(actor A, r *R) error {
		return h.facts.roles.UpdateByID(nil, "t", "r", nil)
	})
}`
	const roleAddOutside = `package command
func (h H) Handle() error { return h.facts.roles.Add(nil, nil) }`
	const submitIsNotAuthority = `package command
func (h H) Handle() error { return h.requests.Add(nil, nil) }`
	for name, c := range map[string]struct {
		src  string
		want int
	}{
		"a membership write outside the writer":     {bypass, 1},
		"a membership write inside the writer":      {viaWriter, 0},
		"a role write inside updateRole":            {viaUpdateRole, 0},
		"a role Add outside the writer":             {roleAddOutside, 1},
		"submitting a request carries no authority": {submitIsNotAuthority, 0},
	} {
		off, _ := authorityWriteOffences("x.go", []byte(c.src), t)
		if len(off) != c.want {
			t.Errorf("%s: %d offences %v, want %d", name, len(off), off, c.want)
		}
	}
}
