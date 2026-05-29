// single_source_arch_test.go — three fitness functions that close the
// "one canonical source for a value/decision" gaps found in the
// May-2026 audit. Each bans a defect CLASS that shipped as a concrete
// instance:
//
//   GATE 1. TestArch_FakeHonorsCommitFlag
//           — a per-aggregate FakeRepository.UpdateByID whose updateFn
//             returns (commit bool, err error) MUST branch on the commit
//             flag (LSP: mirror the pg adapter's no-persist-on-false
//             contract). Three fakes discarded it with `_ = commit`, so
//             a commit=false caller silently saw mutations the real
//             adapter would have rolled back.
//
//   GATE 2. TestArch_GUCNamesSingleSourced
//           — the RLS GUC names "app.tenant_id"/"app.is_platform" are a
//             security seam shared by every set_config writer and every
//             RLS policy. The exact bare string literal may appear in
//             production .go ONLY in package pg's const declaration; a
//             re-typed copy (the rlstest package previously hand-typed
//             its own) is one typo away from desyncing a writer from the
//             policy.
//
//   GATE 3. TestArch_StatusComparisonsUseTypedConst
//           — an adapter comparing `<x>.Status`/`<x>.State` against a
//             string literal ("active") bypasses the typed Status const
//             (membership.StatusActive.String()). A literal drifts
//             independently of the enum; one rename desyncs read from
//             write. Ban the literal; require the typed const.
//
// arch-test:no-synctest — all three are purely-static AST/text analysis;
// no goroutines, no time-bound, no DB.

package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// GATE 1 — TestArch_FakeHonorsCommitFlag
// ============================================================================
//
// Walk every internal/*/domain/*/<aggregate>test/*.go (production, not
// _test.go). For each method named UpdateByID or UpsertWithVersion whose
// signature takes an updateFn parameter returning (bool, error), bind the
// NAME of that bool result's caller variable — i.e. the LHS of the
// `<commit>, <err> := updateFn(...)` (or `:= fn(...)`) assignment — and
// FAIL when the body either:
//
//   - blank-discards it (`_ = <commit>`), OR
//   - never references it in a branch condition (if / switch).
//
// The correct fakes (role/tenant/person) all carry `if !commit { return
// nil }`; the pre-fix rolehierarchy/membership/refreshtoken fakes carried
// `_ = commit`. Requiring a branch on the flag makes the LSP contract
// mechanically enforced.
//
// Scope: production — the fake repositories under <aggregate>test/ are
// imported by app-layer tests but are themselves non-_test.go production
// files (walkGoFiles includeTests=false skips _test.go). Test files may
// invoke updateFn however they like.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (re-add `_ = commit` to one fixed fake
// → this gate goes RED). A committed fixture fake under <aggregate>test/
// would itself be the banned shape (it would have to satisfy the real
// Repository interface) — the AST commit-flag-branch matcher IS the
// fitness function.
func TestArch_FakeHonorsCommitFlag(t *testing.T) {
	t.Parallel()

	type violation struct {
		file   string
		line   int
		method string
		why    string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			// Only the co-located fake packages: .../<aggregate>/<aggregate>test/...
			if !strings.Contains(slash, "test/") {
				return
			}
			fset, f := parseFile(t, path, src)

			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Body == nil {
					continue
				}
				if fd.Name.Name != "UpdateByID" && fd.Name.Name != "UpsertWithVersion" {
					continue
				}
				// Require an updateFn param returning (bool, error). If the
				// method doesn't take such a closure, it's not the
				// load→mutate→persist shape this gate governs.
				commitFnParam := updateFnReturnsBoolError(fd)
				if !commitFnParam {
					continue
				}

				commitVar := commitResultVarName(fd.Body)
				switch {
				case commitVar == "":
					violations = append(violations, violation{
						file: slash, line: fset.Position(fd.Pos()).Line, method: fd.Name.Name,
						why: "could not find the commit-flag result of updateFn(...) — the method must capture and branch on the persist decision",
					})
				case identBlankDiscarded(fd.Body, commitVar):
					violations = append(violations, violation{
						file: slash, line: fset.Position(fd.Pos()).Line, method: fd.Name.Name,
						why: "blank-discards the commit flag (`_ = " + commitVar + "`) — mirror the pg adapter: on (false,nil) persist nothing",
					})
				case !identUsedInBranch(fd.Body, commitVar):
					violations = append(violations, violation{
						file: slash, line: fset.Position(fd.Pos()).Line, method: fd.Name.Name,
						why: "never branches on the commit flag " + commitVar + " — require `if !" + commitVar + " { return nil }` (or equivalent) so commit==false persists nothing",
					})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("%d FakeRepository persist-method(s) ignore the updateFn commit flag — LSP violation: the fake must honor the pg adapter's (false,nil)=no-persist contract:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — %s: %s", v.file, v.line, v.method, v.why)
		}
	}
}

// updateFnReturnsBoolError reports whether the FuncDecl has a parameter
// that is a func type returning exactly (bool, error) — the canonical
// updateFn signature.
func updateFnReturnsBoolError(fd *ast.FuncDecl) bool {
	if fd.Type == nil || fd.Type.Params == nil {
		return false
	}
	for _, p := range fd.Type.Params.List {
		ft, ok := p.Type.(*ast.FuncType)
		if !ok || ft.Results == nil || len(ft.Results.List) != 2 {
			continue
		}
		r0, ok0 := ft.Results.List[0].Type.(*ast.Ident)
		r1, ok1 := ft.Results.List[1].Type.(*ast.Ident)
		if ok0 && ok1 && r0.Name == "bool" && r1.Name == "error" {
			return true
		}
	}
	return false
}

// commitResultVarName finds the first assignment whose RHS is a call to a
// parameter-shaped closure (`<var>, <err> := updateFn(...)` /
// `... = fn(...)`) and returns the NAME bound to the FIRST (bool) result.
// We detect the call structurally: a 2-LHS assignment whose single RHS is
// a CallExpr with an Ident callee (the closure param). Returns "" if no
// such assignment is found.
func commitResultVarName(body *ast.BlockStmt) string {
	out := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if out != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		// Callee must be a bare ident (the closure parameter), not a
		// selector (which would be a repo/db method, not updateFn).
		if _, ok := call.Fun.(*ast.Ident); !ok {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name == "_" {
			return true
		}
		out = id.Name
		return false
	})
	return out
}

// identBlankDiscarded reports whether the body contains a `_ = <name>`
// assignment (the discard idiom the pre-fix fakes used to silence the
// unused commit flag).
func identBlankDiscarded(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "_" {
			return true
		}
		if rhs, ok := as.Rhs[0].(*ast.Ident); ok && rhs.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// identUsedInBranch reports whether `name` appears anywhere in the
// condition of an if-statement or in a switch tag/case — i.e. the value
// actually steers control flow. Walking the condition subtree catches
// `if !commit`, `if commit`, `if commit && x`, `switch commit`, etc.
func identUsedInBranch(body *ast.BlockStmt, name string) bool {
	found := false
	containsIdent := func(expr ast.Expr) bool {
		hit := false
		ast.Inspect(expr, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				hit = true
				return false
			}
			return true
		})
		return hit
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch s := n.(type) {
		case *ast.IfStmt:
			if s.Cond != nil && containsIdent(s.Cond) {
				found = true
			}
		case *ast.SwitchStmt:
			if s.Tag != nil && containsIdent(s.Tag) {
				found = true
			}
		}
		return true
	})
	return found
}

// ============================================================================
// GATE 2 — TestArch_GUCNamesSingleSourced
// ============================================================================
//
// The exact string literals "app.tenant_id"/"app.is_platform" are the
// RLS GUC names. They are a two-sided security seam: every set_config
// WRITER and every RLS policy's current_setting() READER must agree. The
// single source of truth is package pg's const declaration
// (pg.GUCTenantID / pg.GUCIsPlatform). This gate fails if the EXACT bare
// literal appears in any production .go BasicLit outside that one file.
//
// Exact-match (not substring) is deliberate: SQL strings legitimately
// embed the name via a parameter now, and `app.is_platform()` (the SQL
// function form) is a different token — neither is a bare GUC literal.
//
// Migrations are SQL, out of scope for this Go gate (a separate SQL gate
// would assert the migration current_setting() refs equal the pg const).
//
// Scope: production — set_config writers + their test helpers (rlstest)
// are non-_test.go production files; _test.go files may spell GUCs freely.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (re-type "app.tenant_id" anywhere →
// RED). A committed fixture file re-typing the literal would itself be
// the banned shape; the exact-literal matcher IS the fitness function.
func TestArch_GUCNamesSingleSourced(t *testing.T) {
	t.Parallel()

	gucLiterals := map[string]bool{
		"app.tenant_id":   true,
		"app.is_platform": true,
	}
	// The single allowed home: package pg's tenancy.go const declaration.
	allowedFile := pathToSlash(filepath.Join(internalDir(t), "common", "pg", "tenancy.go"))

	type violation struct {
		file string
		line int
		lit  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slash := pathToSlash(path)
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val := strings.Trim(lit.Value, "`\"")
			if !gucLiterals[val] {
				return true
			}
			if slash == allowedFile {
				return true // the single source of truth
			}
			violations = append(violations, violation{
				file: slash, line: fset.Position(lit.Pos()).Line, lit: val,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d production .go GUC-name literal(s) outside package pg's const declaration — single-source the RLS GUC names via pg.GUCTenantID / pg.GUCIsPlatform so a writer can't desync from the RLS policy:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — re-typed %q (reference pg.GUC* instead)", v.file, v.line, v.lit)
		}
	}
}

// ============================================================================
// GATE 3 — TestArch_StatusComparisonsUseTypedConst
// ============================================================================
//
// In internal/*/adapters/*.go (production), ban an equality/inequality
// comparison where one side is a `.Status` or `.State` selector (a row
// field / column) and the other side is a STRING LITERAL — e.g.
// `row.Status != "active"`. A typed Status enum owns the canonical
// string form (membership.StatusActive.String()); comparing against a
// literal lets read drift from write on a rename.
//
// Conservative by design: only a `<x>.Status`/`<x>.State` SELECTOR
// compared to a string literal flags. A typed const's .String() call,
// an enum value, or a non-Status field never trips it.
//
// Scope: production — adapters translate DB rows to domain; the typed
// const lives in domain. _test.go files may compare literals freely.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (re-add `row.Status != "active"` →
// RED). A committed fixture adapter with the literal compare would itself
// be the banned shape; the AST selector-vs-literal matcher IS the
// fitness function.
func TestArch_StatusComparisonsUseTypedConst(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		expr string
	}
	var violations []violation

	isStatusSelector := func(e ast.Expr) (string, bool) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		if sel.Sel.Name == "Status" || sel.Sel.Name == "State" {
			base := "?"
			if id, ok := sel.X.(*ast.Ident); ok {
				base = id.Name
			}
			return base + "." + sel.Sel.Name, true
		}
		return "", false
	}
	isStringLit := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING
	}

	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			if strings.Contains(slash, "/db/") { // sqlc-generated
				return
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				bin, ok := n.(*ast.BinaryExpr)
				if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
					return true
				}
				var selName string
				var matched bool
				if sn, ok := isStatusSelector(bin.X); ok && isStringLit(bin.Y) {
					selName, matched = sn, true
				}
				if sn, ok := isStatusSelector(bin.Y); ok && isStringLit(bin.X) {
					selName, matched = sn, true
				}
				if matched {
					violations = append(violations, violation{
						file: slash, line: fset.Position(bin.Pos()).Line, expr: selName,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Errorf("%d adapter Status/State comparison(s) against a string literal — compare against the typed const's .String() (e.g. membership.StatusActive.String()) so read can't drift from write:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — %s {==,!=} \"<literal>\"", v.file, v.line, v.expr)
		}
	}
}
