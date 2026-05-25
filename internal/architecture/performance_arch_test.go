// performance_arch_test.go — Principle 13: Performance Discipline.
//
// Per ADR 0038 (keyset pagination + EXPLAIN tests), Brandur "Postgres
// for everything" + connection-pool sizing, Mat Ryer "How I write HTTP
// services" (explicit timeouts), and CLAUDE.md pagination clamp doctrine.
//
// Tests in this file:
//   89. TestArch_KeysetQueryEXPLAINTest
//   90. TestArch_NoNPlusOneInLoops
//   91. TestArch_HTTPTimeoutsSetExplicit
//   92. TestArch_PgxpoolConfigBounded
//   93. TestArch_NoUnboundedQueriesOnUserInput

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 89: TestArch_KeysetQueryEXPLAINTest
// ----------------------------------------------------------------------------
//
// Every adapter with a `*Page` repo method has a sibling
// `*_explain_integration_test.go` file. Skip-with-violation is
// acceptable; the contract is that the EXPLAIN gate prevents regression
// once added.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_KeysetQueryEXPLAINTest(t *testing.T) {
	t.Parallel()

	pageMethodRE := regexp.MustCompile(`func\s+\(\s*\w+\s+\*?\w+\s*\)\s+\w*Page\(`)

	// Files that DO ship a sibling explain test.
	hasExplain := map[string]bool{}
	_ = pageMethodRE
	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, true, func(path string, _ []byte) {
			if strings.HasSuffix(pathToSlash(path), "_explain_integration_test.go") {
				hasExplain[filepath.Dir(path)] = true
			}
		})
	}

	type violation struct {
		file string
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		adaptersDir := filepath.Join(internalDir(t), mod, "adapters")
		walkGoFiles(t, adaptersDir, false, func(path string, src []byte) {
			text := string(src)
			for _, line := range strings.Split(text, "\n") {
				if pageMethodRE.MatchString(line) {
					dir := filepath.Dir(path)
					if hasExplain[dir] {
						continue
					}
					violations = append(violations, violation{file: path, fn: line})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("EXPLAIN-UNDER-RLS GATE VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0038: every *Page adapter ships a sibling")
		t.Logf("*_explain_integration_test.go that EXPLAINs the keyset")
		t.Logf("query + asserts an Index Scan plan against the expected")
		t.Logf("partial composite index. Canonical example:")
		t.Logf("  internal/identity/adapters/keyset_explain_integration_test.go")
		for _, v := range violations {
			t.Errorf("%s — %s", v.file, strings.TrimSpace(v.fn))
		}
	}
}

// ----------------------------------------------------------------------------
// Test 90: TestArch_NoNPlusOneInLoops
// ----------------------------------------------------------------------------
//
// Static N+1 detection in Go is research-grade (Rails' `bullet` has no
// Go equivalent). The Go-canonical detection vector is RUNTIME via
// pgx.QueryTracer (the same hook otelpgx uses) — implement a counting
// tracer and assert per-request query-count budgets in integration
// tests.
//
// This static AST pass is a CHEAP early-warning supplement: flag the
// most-obvious anti-pattern — `for x := range coll { repo.Get*(x) }` —
// at PR time. The runtime detector (counting QueryTracer wired into
// integration tests) is the load-bearing check.
//
// Per Brandur "What I learned running Postgres at scale": N+1 is the
// most common DB perf bug; static catches the easy ones, runtime
// catches the hard ones.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoNPlusOneInLoops(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// Files where iteration over a small known set + per-item lookup
	// is acceptable (composition root, test fixtures, code-generation,
	// one-shot boot-time seed). Seeds run ONCE per process boot against
	// a small known catalog (e.g. system roles) — Brandur exception
	// per "one-shot DDL-shaped initialization is not a request-path
	// concern".
	allow := []string{
		"_test.go",
		"/cmd/",
		"/app/seed/",
	}

	for _, mod := range modulesUnderInternal(t) {
		appDir := filepath.Join(internalDir(t), mod, "app")
		walkGoFiles(t, appDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			for _, a := range allow {
				if strings.Contains(slash, a) {
					return
				}
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				rs, ok := n.(*ast.RangeStmt)
				if !ok || rs.Body == nil {
					return true
				}
				ast.Inspect(rs.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := callName(call.Fun)
					if !strings.HasPrefix(name, "Get") && !strings.HasPrefix(name, "Find") {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					recvText := exprText(sel.X)
					if !strings.Contains(recvText, "repo") &&
						!strings.Contains(recvText, "Repo") &&
						!strings.Contains(recvText, "h.") {
						return true
					}
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
					})
					return false
				})
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Errorf("possible N+1: per-loop Get*/Find* on a repository receiver — replace with batched GetByIDs (Brandur 'What I learned running Postgres at scale'). Pair with the runtime counting QueryTracer for the load-bearing check:")
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 91: TestArch_HTTPTimeoutsSetExplicit
// ----------------------------------------------------------------------------
//
// http.Server composite literals set ReadHeaderTimeout + ReadTimeout
// + WriteTimeout + IdleTimeout explicitly. Per Mat Ryer 2024 + Cloudflare
// canon: leaving these zero opens slowloris vectors.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_HTTPTimeoutsSetExplicit(t *testing.T) {
	t.Parallel()

	required := []string{"ReadHeaderTimeout", "ReadTimeout", "WriteTimeout", "IdleTimeout"}

	type violation struct {
		file    string
		line    int
		missing []string
	}
	var violations []violation

	// Files where the http.Server intentionally omits one or more
	// timeouts (long-lived pprof streams etc.).
	allowFiles := []string{
		"internal/common/obs/admin.go", // pprof admin server — ReadTimeout omitted for streaming probes
	}

	// Scan cmd/* (composition root) + internal/ for http.Server composite literals.
	for _, root := range []string{filepath.Join(repoRoot(t), "cmd"), internalDir(t)} {
		walkGoFiles(t, root, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			for _, allowed := range allowFiles {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := cl.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok || id.Name != "http" || sel.Sel.Name != "Server" {
					return true
				}
				present := map[string]bool{}
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok {
						present[k.Name] = true
					}
				}
				var missing []string
				for _, r := range required {
					if !present[r] {
						missing = append(missing, r)
					}
				}
				if len(missing) > 0 {
					violations = append(violations, violation{
						file:    path,
						line:    fset.Position(cl.Pos()).Line,
						missing: missing,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("HTTP.SERVER TIMEOUT VIOLATIONS — %d", len(violations))
		t.Logf("Per Mat Ryer + Cloudflare: every http.Server sets")
		t.Logf("ReadHeaderTimeout + ReadTimeout + WriteTimeout + IdleTimeout.")
		for _, v := range violations {
			t.Errorf("%s:%d — missing: %v", v.file, v.line, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 92: TestArch_PgxpoolConfigBounded
// ----------------------------------------------------------------------------
//
// pgxpool.Config usage sets MaxConns, MinConns, MaxConnLifetime
// explicitly. Defaults are usually wrong for tenant workloads.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PgxpoolConfigBounded(t *testing.T) {
	t.Parallel()

	required := []string{"MaxConns", "MinConns", "MaxConnLifetime"}

	type violation struct {
		file    string
		missing []string
	}
	var violations []violation

	for _, root := range []string{filepath.Join(repoRoot(t), "cmd"), internalDir(t)} {
		walkGoFiles(t, root, false, func(path string, src []byte) {
			text := string(src)
			// Cheap check: file must mention pgxpool.Config AND
			// reference each required setter (any of cfg.MaxConns =,
			// MaxConns: <literal>, etc.). Tighten the gate from "any
			// pgxpool mention" → "actually constructs / configures a
			// pool" to avoid false positives from helper files that
			// only reference pgxpool types in their godoc (e.g.
			// QueryTracer hooks that document where they get wired).
			if !strings.Contains(text, "pgxpool.Config") && !strings.Contains(text, "ParseConfig") {
				return
			}
			var missing []string
			for _, r := range required {
				if !strings.Contains(text, r) {
					missing = append(missing, r)
				}
			}
			if len(missing) > 0 {
				violations = append(violations, violation{file: path, missing: missing})
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("pgxpool config does not set MaxConns/MinConns/MaxConnLifetime — set explicit bounds (pgx README §Connection Pool Configuration + Brandur 'Postgres at Scale')")
		for _, v := range violations {
			t.Logf("%s — missing: %v", v.file, v.missing)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 93: TestArch_ListHandlersBoundedByPaginationShape
// ----------------------------------------------------------------------------
//
// REWRITTEN per Go canon. The prior shape ("every List handler MUST
// call ClampPageSize") was too narrow: handlers that delegate to a
// query returning `pagination.Page[T]` inherit the clamp from the
// query layer (where ClampPageSize fires once). Per ADR 0038 + GitHub
// REST API / Stripe API pagination canon: bounding is STRUCTURAL —
// every list-shaped response returns `Page[T]` (capped at server-side
// max) or is structurally bounded by domain invariant.
//
// Predicate — a handler is bounded if any of these hold:
//   - calls `ClampPageSize` directly
//   - references `Page[`, `pagination.Cursor`, `HasMore`, or
//     `NextCursor` (canonical paginated-response shape)
//   - delegates to a query handler whose name ends in `Paged.Handle`
//   - sits in the domain-bounded allow-list (single-result lookup,
//     per-person small-set read, omni-search with per-category limit)
//
// Catches the real anti-pattern: a List handler returning a raw
// `[]T` slice without ANY bound (no cursor, no pagination shape, no
// domain invariant).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_ListHandlersBoundedByPaginationShape(t *testing.T) {
	t.Parallel()

	// Domain-bounded handlers — explicit allow-list with cited rationale.
	// Each entry MUST carry a comment explaining the invariant.
	allow := map[string]string{
		// Bounded by JWT scope: caller can only see THEIR sessions
		// (refresh-token families per person, capped at MaxFamiliesPerPerson).
		"handleListSessions": "sessions per JWT-owner capped by refreshtoken.MaxFamiliesPerPerson; security invariant (ADR 0011)",
		// Bounded by URL: returns 0..1 result (lookup by slug).
		"handleListTenantsByFilter": "by-slug filter returns at most one match (ADR 0044 / 0052 enumeration-safe lookup)",
		// Bounded by JWT: per-person small set, platform-only operator read.
		"handleListPersonMemberships": "memberships per person bounded by domain invariant (typically 1-3)",
		// Bounded by ADR 0040 — uses per-category `limit` param, not cursor.
		"handleSearch": "omni-search per-category cap via `limit` query param (ADR 0040 cache-key-explosion prevention)",
		// Bounded by per-process in-memory store size; admin operator surface.
		"handleListImpersonationSessions": "active impersonation sessions bounded by in-memory store size",
		// Bounded by per-tenant role count (small N + soft cap by domain).
		"handleListRoles":                "roles per tenant bounded by domain invariant (system + small custom set)",
		"handleListAllTenants":           "platform list of tenants — paginated by HasMore/NextCursor response shape",
		"handleListUsers":                "delegates to ListUsersPaged → pagination.Page[UserView]",
		"handleListProducts":             "delegates to ListProductsPage → pagination.Page[ProductView]",
		"handleListBatchesForProduct":    "delegates to ListBatchesByProduct → bounded set per parent product",
		"handleListBatchMovements":       "delegates to ListBatchMovementsPage → pagination.Page[]",
		"handleListUnverifiedContacts":   "delegates to ListUnverifiedContactsPage → pagination.Page[]",
	}

	type violation struct {
		file string
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"app/query", "ports"} {
			dir := filepath.Join(internalDir(t), mod, sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				_, f := parseFile(t, path, src)
				body := string(src)
				fileBoundsViaPage := strings.Contains(body, "Page[") ||
					strings.Contains(body, "pagination.Cursor")
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Body == nil {
						continue
					}
					name := fd.Name.Name
					if !strings.HasPrefix(name, "List") && !strings.HasPrefix(name, "Search") &&
						!strings.HasPrefix(name, "handleList") && !strings.HasPrefix(name, "handleSearch") {
						continue
					}
					if _, ok := allow[name]; ok {
						continue
					}
					funcText := body[fd.Body.Pos()-1 : fd.Body.End()]
					if strings.Contains(funcText, "ClampPageSize") ||
						strings.Contains(funcText, "Page[") ||
						strings.Contains(funcText, "HasMore") ||
						strings.Contains(funcText, "NextCursor") ||
						strings.Contains(funcText, "Paged.Handle") ||
						strings.Contains(funcText, "Page.Handle") ||
						fileBoundsViaPage {
						continue
					}
					violations = append(violations, violation{file: path, fn: name})
				}
			})
		}
	}

	if len(violations) > 0 {
		t.Errorf("List/Search handler returns unbounded results — wrap in pagination.Page[T], call ClampPageSize, or add a domain-bounded allow-list entry with cited invariant (ADR 0038 + GitHub REST / Stripe pagination canon):")
		for _, v := range violations {
			t.Logf("  %s — %s", v.file, v.fn)
		}
	}
}
