// shared_fixture_arch_test.go — Fitness function for the shared
// testcontainer + pool pattern (Brandur / TDL canon, see
// internal/common/pgtest/).
//
// Background: tests sharing a Postgres pool per package fall into two
// MUTUALLY EXCLUSIVE buckets:
//
//   - Tenant-scoped:  uses fresh tenant.ID per test + RLS-bound ctx,
//                     calls t.Parallel(). Safe to run concurrently with
//                     siblings — RLS isolates rows by tenant.
//   - Cross-tenant:   reads/writes across tenants (outbox forwarder,
//                     platform-scope queries, count-all-rows asserts).
//                     Calls sharedPG.TruncateAll(t) to reset state; runs
//                     in Phase 1 SERIAL (NO t.Parallel).
//
// Go's testing package executes ALL non-parallel tests first (Phase 1)
// then resumes parallel tests together (Phase 2). The two buckets above
// land in opposite phases by design — they NEVER overlap, no race.
//
// THE FOOTGUN: a test that calls BOTH t.Parallel() AND TruncateAll(t)
// joins Phase 2 (parallel) while wiping shared state. Other parallel
// tests' inserts/queries observe the truncation mid-flight → silent
// data loss + flake. This is documentation-only today; the test below
// makes it mechanical.

package architecture_test

import (
	"go/ast"
	"strings"
	"testing"
)

// TestArch_TruncateAllImpliesSerial asserts that no integration test
// calls BOTH t.Parallel() AND a *.TruncateAll(t) helper. TruncateAll
// mutates shared package-scoped state (the testcontainer DB) — it
// MUST run in Phase 1 (serial) so it doesn't race with parallel
// tests' RLS-isolated work in Phase 2.
//
// Per Go's two-phase test scheduling (testing.T.Parallel docs):
// non-parallel tests run sequentially first; parallel tests resume
// together afterwards. Mixing the two patterns in a single test
// puts it in the wrong phase + breaks the invariant.
//
// arch-test:no-negative-fixture — the assertion target is every
// integration test in the live tree. A synthetic fixture would
// require parsing a fake file path; the test IS the gate.
func TestArch_TruncateAllImpliesSerial(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		fn   string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if !isIntegrationTestFile(src) && !strings.HasSuffix(slash, "_integration_test.go") {
			return
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if name == "TestMain" {
				continue
			}
			if !strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "TestArch_") || strings.HasPrefix(name, "TestMeta_") {
				continue
			}
			callsParallel := false
			callsTruncateAll := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// t.Parallel() — receiver is t or tt; method Parallel.
				if id, ok := sel.X.(*ast.Ident); ok &&
					(id.Name == "t" || id.Name == "tt") &&
					sel.Sel.Name == "Parallel" {
					callsParallel = true
				}
				// <anything>.TruncateAll(t) — any receiver, method
				// TruncateAll. First arg should be a *testing.T (we
				// don't deeply check; the method name is the signal).
				if sel.Sel.Name == "TruncateAll" {
					callsTruncateAll = true
				}
				return true
			})
			if callsParallel && callsTruncateAll {
				violations = append(violations, violation{
					file: slash,
					fn:   name,
					line: fset.Position(fd.Pos()).Line,
				})
			}
		}
	})

	if len(violations) == 0 {
		return
	}

	t.Errorf("%d test func(s) call BOTH t.Parallel() AND *.TruncateAll(t) — these patterns are mutually exclusive (see feedback_parallel_truncate_exclusion in memory).", len(violations))
	t.Logf("TruncateAll mutates the shared testcontainer DB; t.Parallel makes the test")
	t.Logf("run in Phase 2 alongside RLS-isolated tests, which observe the truncate")
	t.Logf("mid-flight + fail/flake. Fix: drop ONE of the two.")
	t.Logf("  - If the test is cross-tenant (outbox forwarder, platform-scope):")
	t.Logf("    keep TruncateAll, REMOVE t.Parallel — it'll run in Phase 1 (serial).")
	t.Logf("  - If the test is tenant-scoped (use a fresh tenant.ID + RLS):")
	t.Logf("    keep t.Parallel, REMOVE TruncateAll — RLS provides isolation.")
	t.Logf("  - If you genuinely need a cross-tenant test to run in parallel:")
	t.Logf("    use per-test template-DB isolation instead of TruncateAll.")
	for _, v := range violations {
		t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
	}
}

