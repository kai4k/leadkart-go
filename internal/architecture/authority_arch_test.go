// authority_arch_test.go — ADR 0069: every write of authority state in the
// identity module goes through the authority writer. Authority state is role
// assignments, permission overrides, membership status and manager links
// (membership.Repository), roles and their permissions (role.Repository) and
// permission-request decisions (permissionrequest.Repository).
//
// The scan is TYPED, not lexical: each package of internal/identity is parsed and
// type-checked with go/parser + go/types (the route scanner's approach, in
// route_scan_test.go: the stub importer fakes every foreign package by path; this
// file adds a source importer for the identity DOMAIN packages so the repository
// interfaces and the aggregates are real types). A call is a repository write
// because the method it resolves to is declared by one of those interfaces, not
// because of what the receiver variable is called or how it was reached (alias,
// struct field, embedded wrapper, parameter).
//
// What it enforces, exactly:
//
//  1. Outside internal/identity/app/command (and the adapters, which implement
//     the repositories) NOTHING calls Add or UpdateByID on those repositories:
//     not the ports, not the queries, not a package added tomorrow, except the
//     one pinned seeding of a brand-new tenant's default roles.
//  2. Inside the command package, each write has ONE permitted home:
//     membership UpdateByID in authorityWriter.updateMember; role UpdateByID in
//     roleWriter.updateRole; permission-request UpdateByID in
//     authorityWriter.decideRequest. Every other write is a pinned exception
//     (key "<repo>.<method> <file>:<Owner.func>", with its reason), asserted
//     EXACTLY: an exception that matches no write fails.
//  3. The writer entries (locked, updateMember, updateRole) are the methods of
//     authorityWriter / roleWriter, found by object, so a function that merely
//     is NAMED locked is not one. A pinned write that is not the registration of
//     a brand-new tenant must sit inside a function literal handed to an entry.
//  4. decideRequest is only called inside such a literal.
//  5. A function that adds a membership never also assigns it roles or overrides:
//     a new membership carries no authority.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const identityModulePath = "github.com/leadkart/leadkart-go/"

// domainImportPrefix: packages the typed scan loads from source. Every other
// import is faked by path (scanImporter), exactly like the route scanner.
const domainImportPrefix = identityModulePath + "internal/identity/domain/"

// authorityRepo names one of the three repositories whose writes are authority.
type authorityRepo struct {
	pkg    string   // the domain package that declares Repository
	label  string   // how an offence names it
	writes []string // the methods that persist state
}

var authorityRepos = []authorityRepo{
	{pkg: domainImportPrefix + "membership", label: "membership", writes: []string{"Add", "UpdateByID"}},
	{pkg: domainImportPrefix + "role", label: "role", writes: []string{"Add", "UpdateByID"}},
	{pkg: domainImportPrefix + "permissionrequest", label: "permissionrequest", writes: []string{"Add", "UpdateByID"}},
}

// writerEntries are the methods whose function-literal arguments run inside the
// authority writer (one transaction, the tenant authority lock, the fresh actor).
var writerEntries = []string{"locked", "updateMember", "updateRole"}

// writerReceivers are the only receiver types of those methods.
var writerReceivers = []string{"authorityWriter", "roleWriter"}

// nativeWriteHomes: the one function each write lives in. Key "<repo>.<method>",
// value "Receiver.func" of the enclosing declaration.
var nativeWriteHomes = map[string]string{
	"membership.UpdateByID":        "authorityWriter.updateMember",
	"role.UpdateByID":              "roleWriter.updateRole",
	"permissionrequest.UpdateByID": "authorityWriter.decideRequest",
}

// writePin is a write outside its native home, with why that is safe.
type writePin struct {
	reason string
	locked bool // it must sit inside a function literal handed to a writer entry
}

// authorityWritePins is asserted EXACTLY (a pin that matches no write fails), so
// a new bypass cannot hide behind an old reason.
var authorityWritePins = map[string]writePin{
	"membership.UpdateByID user_authz.go:AssignUserRoleHandler.Handle":                       {"assigns a role: loads the role before the membership row so a missing role answers first; decided by MayAssignRole under the lock", true},
	"membership.UpdateByID user_authz.go:RevokeUserRoleHandler.Handle":                       {"revokes a role: same shape as assign; decided by MayRevokeRole under the lock", true},
	"membership.UpdateByID user_authz.go:AssignUserManagerHandler.Handle":                    {"sets a manager: loads the proposed manager and walks its chain before the row; decided by MayReplaceManager under the lock", true},
	"membership.UpdateByID permission_request.go:ApprovePermissionRequestHandler.Handle":     {"writes the approved grant on the requester's row inside the approval's locked transaction", true},
	"membership.UpdateByID role_management.go:DeleteRoleHandler.Handle":                      {"removes a deleted role from its INACTIVE holders inside updateRole (an active holder refuses the delete)", true},
	"membership.UpdateByID register_tenant.go:RegisterTenantHandler.seedRolesAndAssignOwner": {"the first Company Owner of a brand-new tenant: no other member, no actor, nothing to decide", false},
	"membership.Add register_tenant.go:RegisterTenantHandler.createMembership":               {"the first member of a brand-new tenant: no actor, carries no roles (the owner role is assigned in seedRolesAndAssignOwner)", false},
	"role.Add seed/role_catalog.go:ApplyDefaultRoles":                                        {"seeds the default role catalogue of a brand-new tenant (RegisterTenant, before any member exists); it is not callable from a route", false},
	"membership.Add user_create.go:CreateUserHandler.createMembership":                       {"a new member carries no roles and no overrides; roles are assigned later through AssignUserRole", false},
	"role.Add role_management.go:CreateRoleHandler.Handle":                                   {"a new role has no holders; decided by MayCreateRole under the lock", true},
	"permissionrequest.Add permission_request.go:RequestPermissionElevationHandler.Handle":   {"submitting a request carries no authority; the decision is a different write", false},
}

// sourceImporter type-checks the identity domain packages from source and fakes
// everything else like the route scanner.
type sourceImporter struct {
	t     *testing.T
	root  string
	fset  *token.FileSet
	cache map[string]*types.Package
}

func newSourceImporter(t *testing.T) *sourceImporter {
	t.Helper()
	return &sourceImporter{t: t, root: repoRoot(t), fset: token.NewFileSet(), cache: map[string]*types.Package{}}
}

func (im *sourceImporter) Import(p string) (*types.Package, error) {
	if !strings.HasPrefix(p, domainImportPrefix) {
		return scanImporter{}.Import(p)
	}
	if pkg, ok := im.cache[p]; ok {
		return pkg, nil
	}
	dir := filepath.Join(im.root, filepath.FromSlash(strings.TrimPrefix(p, identityModulePath)))
	files := im.parseDir(dir)
	conf := types.Config{Importer: im, Error: func(error) {}, FakeImportC: true}
	pkg, _ := conf.Check(p, im.fset, files, nil)
	im.cache[p] = pkg
	return pkg, nil
}

// parseDir parses the non-test .go files of one directory.
func (im *sourceImporter) parseDir(dir string) []*ast.File {
	im.t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		im.t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			im.t.Fatalf("read %s: %v", e.Name(), err)
		}
		f, err := parser.ParseFile(im.fset, filepath.Join(dir, e.Name()), src, 0)
		if err != nil {
			im.t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}
	return files
}

// scannedPkg is one type-checked package under internal/identity.
type scannedPkg struct {
	path  string // import path
	pkg   *types.Package
	fset  *token.FileSet
	info  *types.Info
	files []*ast.File
}

func (im *sourceImporter) check(importPath string, files []*ast.File) *scannedPkg {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: im, Error: func(error) {}, FakeImportC: true}
	pkg, _ := conf.Check(importPath, im.fset, files, info)
	return &scannedPkg{path: importPath, pkg: pkg, fset: im.fset, info: info, files: files}
}

// repoWrite reports which authority repository write fn is (false when it is
// not one): fn must be a method declared by that package's Repository interface.
func repoWrite(fn *types.Func) (repo authorityRepo, method string, ok bool) {
	if fn == nil || fn.Pkg() == nil {
		return authorityRepo{}, "", false
	}
	for _, r := range authorityRepos {
		if fn.Pkg().Path() != r.pkg || !slices.Contains(r.writes, fn.Name()) {
			continue
		}
		obj := fn.Pkg().Scope().Lookup("Repository")
		if obj == nil {
			continue
		}
		iface, isIface := obj.Type().Underlying().(*types.Interface)
		if !isIface {
			continue
		}
		for i := range iface.NumMethods() {
			if iface.Method(i) == fn {
				return r, fn.Name(), true
			}
		}
	}
	return authorityRepo{}, "", false
}

// recvTypeOf returns the (pointer-stripped) named type of fn's receiver.
func recvTypeOf(fn *types.Func) *types.Named {
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return nil
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, _ := t.(*types.Named)
	return n
}

// isWriterEntry: call is a call of authorityWriter/roleWriter .locked / .updateMember
// / .updateRole of THIS package (found by object, so a lookalike name is not one).
func (p *scannedPkg) isWriterEntry(call *ast.CallExpr) bool {
	se, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	sel := p.info.Selections[se]
	if sel == nil {
		return false
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok || fn.Pkg() != p.pkg || !slices.Contains(writerEntries, fn.Name()) {
		return false
	}
	n := recvTypeOf(fn)
	return n != nil && slices.Contains(writerReceivers, n.Obj().Name())
}

// isMethodOf: call resolves to method name of the named type recv declared in this package.
func (p *scannedPkg) isMethodOf(call *ast.CallExpr, recv, name string) bool {
	se, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	sel := p.info.Selections[se]
	if sel == nil {
		return false
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok || fn.Pkg() != p.pkg || fn.Name() != name {
		return false
	}
	n := recvTypeOf(fn)
	return n != nil && n.Obj().Name() == recv
}

// isMembershipMutator: a call of Membership.AssignRole or
// Membership.ReplacePermissionOverrides (a new membership must not receive them).
func (p *scannedPkg) isMembershipMutator(call *ast.CallExpr) bool {
	se, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	sel := p.info.Selections[se]
	if sel == nil {
		return false
	}
	fn, ok := sel.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != domainImportPrefix+"membership" {
		return false
	}
	return fn.Name() == "AssignRole" || fn.Name() == "ReplacePermissionOverrides"
}

// ownerOf names a function declaration "Receiver.func" (or "func").
func ownerOf(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		return identRecvName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func identRecvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return identRecvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return identRecvName(x.X)
	}
	return "?"
}

// authorityWriteOffences scans one package. isCommand is whether it is
// internal/identity/app/command (the only package that may write). It returns the
// offences ("<file>:<line> <what>") and the pins it used.
func authorityWriteOffences(p *scannedPkg, isCommand bool) (offences, usedPins []string) {
	for _, f := range p.files {
		base := filepath.Base(p.fset.Position(f.Pos()).Filename)
		for _, decl := range f.Decls {
			var body ast.Node
			owner := "<package var>"
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				body, owner = d.Body, ownerOf(d)
			case *ast.GenDecl:
				body = d
			default:
				continue
			}
			var addsMembership, mutatesMembership bool
			var addPos token.Pos
			var walk func(n ast.Node, inside bool)
			walk = func(n ast.Node, inside bool) {
				ast.Inspect(n, func(c ast.Node) bool {
					call, ok := c.(*ast.CallExpr)
					if !ok {
						return true
					}
					entry := p.isWriterEntry(call)
					walk(call.Fun, inside)
					for _, a := range call.Args {
						if lit, ok := a.(*ast.FuncLit); ok {
							walk(lit.Body, inside || entry)
						} else {
							walk(a, inside)
						}
					}
					pos := p.fset.Position(call.Pos())
					at := base + ":" + strconv.Itoa(pos.Line)
					if p.isMembershipMutator(call) {
						mutatesMembership = true
					}
					if p.isMethodOf(call, "authorityWriter", "decideRequest") && !inside {
						offences = append(offences, at+" decideRequest outside authorityWriter.locked/updateMember/updateRole in "+owner+" — a request decision must run under the tenant lock")
					}
					if se, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
						if sel := p.info.Selections[se]; sel != nil {
							if fn, ok := sel.Obj().(*types.Func); ok {
								if repo, method, isWrite := repoWrite(fn); isWrite {
									if repo.label == "membership" && method == "Add" {
										addsMembership, addPos = true, call.Pos()
									}
									file := base // pins in the command package are keyed by file; elsewhere by package/file
									if !isCommand {
										file = path.Base(p.path) + "/" + base
									}
									key := repo.label + "." + method + " " + file + ":" + owner
									pin, pinned := authorityWritePins[key]
									switch {
									case isCommand && nativeWriteHomes[repo.label+"."+method] == owner:
										// the one function this write lives in
									case !pinned && !isCommand:
										offences = append(offences, at+" "+repo.label+"."+method+" in "+p.path+" "+owner+" — authority is written by commands only (ADR 0069)")
									case !pinned:
										offences = append(offences, at+" "+repo.label+"."+method+" in "+owner+" — a write of authority state outside its home and outside the pinned exceptions: it skips the tenant lock, the fresh actor or the policy (ADR 0069)")
									case pin.locked && !inside:
										offences = append(offences, at+" "+repo.label+"."+method+" in "+owner+" — pinned, but not inside a function literal handed to authorityWriter.locked/updateMember/updateRole")
									default:
										usedPins = append(usedPins, key)
									}
								}
							}
						}
					}
					return false // arguments handled above
				})
			}
			walk(body, false)
			if addsMembership && mutatesMembership {
				offences = append(offences, base+":"+strconv.Itoa(p.fset.Position(addPos).Line)+" membership.Add in "+owner+" together with AssignRole / ReplacePermissionOverrides — a new membership carries no authority")
			}
		}
	}
	return offences, usedPins
}

// identityPackageDirs lists every directory under internal/identity that holds a
// non-test Go package, except the adapters (they implement the repositories).
func identityPackageDirs(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(internalDir(t), "identity")
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "adapters" {
			return filepath.SkipDir
		}
		entries, rerr := os.ReadDir(path)
		if rerr != nil {
			return rerr
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				dirs = append(dirs, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk identity: %v", err)
	}
	return dirs
}

// TestArch_AuthorityWritesGoThroughTheAuthorityWriter enforces the rules in the
// file comment over every package of internal/identity, by type.
//
// Scope: production — non-test files of internal/identity except the adapters.
//
// arch-test:no-negative-fixture (RED proof: TestAuthorityWriteScanner_FlagsEveryBypass feeds the scanner one synthetic offender per bypass)
func TestArch_AuthorityWritesGoThroughTheAuthorityWriter(t *testing.T) {
	t.Parallel()
	im := newSourceImporter(t)
	commandDir := filepath.Join(internalDir(t), "identity", "app", "command")
	var used []string
	scanned := 0
	for _, dir := range identityPackageDirs(t) {
		rel, err := filepath.Rel(repoRoot(t), dir)
		if err != nil {
			t.Fatal(err)
		}
		importPath := identityModulePath + filepath.ToSlash(rel)
		files := im.parseDir(dir)
		if len(files) == 0 {
			continue
		}
		scanned++
		isCommand := filepath.Clean(dir) == filepath.Clean(commandDir)
		off, pins := authorityWriteOffences(im.check(importPath, files), isCommand)
		used = append(used, pins...)
		for _, o := range off {
			t.Errorf("%s", o)
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned only %d identity packages; the walk is broken", scanned)
	}
	for key := range authorityWritePins {
		if !slices.Contains(used, key) {
			t.Errorf("authorityWritePins entry %q matches no write — remove it", key)
		}
	}
}

// ----- the scanner's self-test: one synthetic offender per bypass ---------------

// syntheticPrelude is the command package's writer, shaped like the real one.
const syntheticPrelude = `package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

type authorityFacts struct {
	memberships membership.Repository
	roles       role.Repository
}

type authorityWriter struct{ facts authorityFacts }
type roleWriter struct{ authorityWriter }

func (w authorityWriter) locked(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

func (w authorityWriter) updateMember(ctx context.Context, tenantID string, id membership.ID, fn func(*membership.Membership) (bool, error)) error {
	return w.locked(ctx, func(ctx context.Context) error {
		return w.facts.memberships.UpdateByID(ctx, "", id, fn)
	})
}

func (w roleWriter) updateRole(ctx context.Context, id role.ID, fn func(*role.Role) (bool, error)) error {
	return w.locked(ctx, func(ctx context.Context) error {
		return w.facts.roles.UpdateByID(ctx, "", id, fn)
	})
}

func (w authorityWriter) decideRequest(ctx context.Context, requests permissionrequest.Repository, id permissionrequest.ID, fn func(*permissionrequest.Request) (bool, error)) error {
	return requests.UpdateByID(ctx, "", id, fn)
}

type handler struct {
	w        authorityWriter
	rw       roleWriter
	requests permissionrequest.Repository
}
`

// scanAuthoritySource type-checks prelude + one source as package path and scans it.
func scanAuthoritySource(t *testing.T, importPath string, isCommand bool, src string) []string {
	t.Helper()
	im := newSourceImporter(t)
	var files []*ast.File
	for name, s := range map[string]string{"prelude.go": syntheticPrelude, "x.go": src} {
		if name == "prelude.go" && !isCommand {
			continue
		}
		f, err := parser.ParseFile(im.fset, name, s, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	off, _ := authorityWriteOffences(im.check(importPath, files), isCommand)
	return off
}

func TestAuthorityWriteScanner_FlagsEveryBypass(t *testing.T) {
	t.Parallel()
	const header = "package command\nimport (\n\t\"context\"\n\t\"github.com/leadkart/leadkart-go/internal/identity/domain/membership\"\n\t\"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest\"\n\t\"github.com/leadkart/leadkart-go/internal/identity/domain/role\"\n)\n"
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"a direct membership write outside the writer", header + `
func (h handler) A(ctx context.Context, id membership.ID) error {
	return h.w.facts.memberships.UpdateByID(ctx, "", id, func(m *membership.Membership) (bool, error) { return true, nil })
}`, 1},
		{"an ALIASED receiver (repo := ...; repo.UpdateByID)", header + `
func (h handler) A(ctx context.Context, id membership.ID) error {
	repo := h.w.facts.memberships
	return repo.UpdateByID(ctx, "", id, func(m *membership.Membership) (bool, error) { return true, nil })
}`, 1},
		{"a write through a parameter of the repository type", header + `
func write(ctx context.Context, repo membership.Repository, id membership.ID) error {
	return repo.UpdateByID(ctx, "", id, nil)
}`, 1},
		{"a write through an embedded wrapper that promotes the method", header + `
type spy struct{ membership.Repository }
func (h handler) A(ctx context.Context, id membership.ID, s spy) error { return s.UpdateByID(ctx, "", id, nil) }`, 1},
		{"a free function NAMED locked is not the writer", header + `
func locked(fn func() error) error { return fn() }
func (h handler) A(ctx context.Context, id membership.ID) error {
	return locked(func() error { return h.w.facts.memberships.UpdateByID(ctx, "", id, nil) })
}`, 1},
		{"a method named locked on another type is not the writer", header + `
type other struct{}
func (other) locked(fn func() error) error { return fn() }
func (h handler) A(ctx context.Context, id membership.ID, o other) error {
	return o.locked(func() error { return h.w.facts.memberships.UpdateByID(ctx, "", id, nil) })
}`, 1},
		{"roles.UpdateByID inside locked instead of updateRole", header + `
func (h handler) A(ctx context.Context, id role.ID) error {
	return h.w.locked(ctx, func(ctx context.Context) error { return h.w.facts.roles.UpdateByID(ctx, "", id, nil) })
}`, 1},
		{"roles.Add outside the writer", header + `
func (h handler) A(ctx context.Context, r *role.Role) error { return h.w.facts.roles.Add(ctx, r) }`, 1},
		{"memberships.Add of an aggregate that already carries roles", header + `
func (h handler) A(ctx context.Context, m *membership.Membership, r role.ID) error {
	_ = m.AssignRole(r, timeNow())
	return h.w.facts.memberships.Add(ctx, m)
}
func timeNow() (t time_) { return }
type time_ = struct{}`, 2},
		{"a request decision outside decideRequest", header + `
func (h handler) A(ctx context.Context, id permissionrequest.ID) error {
	return h.requests.UpdateByID(ctx, "", id, nil)
}`, 1},
		{"decideRequest outside the writer", header + `
func (h handler) A(ctx context.Context, id permissionrequest.ID) error {
	return h.w.decideRequest(ctx, h.requests, id, nil)
}`, 1},
	}
	for _, c := range cases {
		if off := scanAuthoritySource(t, identityModulePath+"internal/identity/app/command", true, c.src); len(off) != c.want {
			t.Errorf("%s: %d offences %v, want %d", c.name, len(off), off, c.want)
		}
	}
	// A package that is not the command package may not write at all.
	for name, src := range map[string]string{
		"a new package writing a membership": header + `
func A(ctx context.Context, repo membership.Repository, id membership.ID) error { return repo.UpdateByID(ctx, "", id, nil) }`,
		"a new package adding a role": header + `
func A(ctx context.Context, repo role.Repository, r *role.Role) error { return repo.Add(ctx, r) }`,
		"a new package submitting a request": header + `
func A(ctx context.Context, repo permissionrequest.Repository, r *permissionrequest.Request) error { return repo.Add(ctx, r) }`,
	} {
		if off := scanAuthoritySource(t, identityModulePath+"internal/identity/app/sidecar", false, src); len(off) != 1 {
			t.Errorf("%s: %d offences %v, want 1", name, len(off), off)
		}
	}
	// What must pass: the native homes (the prelude itself), a write inside a literal handed to the
	// writer that is also pinned is checked by the real scan; here only the homes and a read.
	const clean = header + `
func (h handler) Read(ctx context.Context, id membership.ID) (*membership.Membership, error) {
	return h.w.facts.memberships.GetByID(ctx, "", id)
}
func (h handler) Decide(ctx context.Context, id permissionrequest.ID) error {
	return h.w.locked(ctx, func(ctx context.Context) error { return h.w.decideRequest(ctx, h.requests, id, nil) })
}
func (h handler) Update(ctx context.Context, id membership.ID) error {
	return h.w.updateMember(ctx, "", id, func(m *membership.Membership) (bool, error) { return true, nil })
}`
	if off := scanAuthoritySource(t, identityModulePath+"internal/identity/app/command", true, clean); len(off) != 0 {
		t.Errorf("the writer's own homes, a read and a locked decideRequest were flagged: %v", off)
	}
}
