// sql_contract_arch_test.go — Fitness function for ADR 0062's
// "adapter integration tests are SQL-contract-only" rule.
//
// THE RULE: every Test* function in `*_repository_pg_test.go` /
// `*_pg_integration_test.go` files MUST declare WHY it justifies
// the testcontainer cost. Either:
//
//   (a) The file's header doc-block contains "SQL-CONTRACT COVERAGE"
//       (the file's scope is documented at the top — appropriate when
//       most tests in the file share the same SQL-contract category)
//
//   (b) The test body contains an inline `// SQL-contract: <what>`
//       comment naming the specific SQL behavior it asserts (RLS,
//       23505 translation, JSONB round-trip, partial-unique-index,
//       outbox-row write, EXPLAIN-under-RLS index, DB trigger, etc.)
//
// Failure to declare means the test is either:
//   - Duplicating fake-covered unit-test coverage (delete it), OR
//   - Testing something SQL-specific without saying what (add the
//     marker so review can verify the claim)
//
// The rule prevents drift back to "every business rule gets its own
// integration test" which is what produced the 80+ integration test
// baseline before the TDL-canon cleanup.
//
// arch-test:parallel-safe-file — file-walk + regex; no shared state.

package architecture_test

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestArch_AdapterIntegrationTestsHaveSQLContractMarker asserts every
// adapter integration test file (path ending `_repository_pg_test.go`
// or `_pg_integration_test.go`) carries a SQL-contract declaration —
// either file-level or per-test.
//
// Reference shape:
//   - File-level: `// SQL-CONTRACT COVERAGE for this file (ADR 0062 ...)`
//     near the top of the file. See
//     internal/identity/adapters/role_repository_pg_test.go for the
//     canonical pattern.
//   - Per-test: `// SQL-contract: <what>` comment above or inside the
//     Test* func body. See
//     internal/identity/adapters/tenant_repository_pg_test.go's
//     TestTenantRepository_Add_PersistsOutboxEventInSameTx for the
//     canonical pattern.
//
// arch-test:no-negative-fixture — the assertion target is the live
// internal/<module>/adapters/ tree. The test IS the gate.
func TestArch_AdapterIntegrationTestsHaveSQLContractMarker(t *testing.T) {
	t.Parallel()

	root := internalDir(t)

	type unmarkedTest struct {
		file string
		fn   string
		line int
	}
	var violations []unmarkedTest

	fileLevelMarkerRE := regexp.MustCompile(`(?i)SQL[-_ ]CONTRACT[-_ ]COVERAGE`)
	perTestMarkerRE := regexp.MustCompile(`(?i)//\s*SQL[-_ ]contract\s*:`)

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		slash := pathToSlash(path)
		// Scope: only adapter integration test files.
		if !strings.Contains(slash, "/adapters/") {
			return nil
		}
		base := filepath.Base(slash)
		isAdapterIntTest := strings.HasSuffix(base, "_repository_pg_test.go") ||
			strings.HasSuffix(base, "_pg_integration_test.go") ||
			(strings.HasSuffix(base, "_pg_test.go") && strings.Contains(base, "repository"))
		if !isAdapterIntTest {
			return nil
		}
		raw, rerr := os.ReadFile(path) //nolint:gosec // arch-test fixture path
		if rerr != nil {
			t.Errorf("read %s: %v", slash, rerr)
			return nil
		}
		body := string(raw)
		// File-level marker exempts the whole file — the header
		// documents the SQL-contract scope.
		if fileLevelMarkerRE.MatchString(body) {
			return nil
		}
		// Per-test check: every Test* func MUST have an inline
		// `// SQL-contract: ...` marker within the test body or its
		// preceding godoc.
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, body, parser.SkipObjectResolution|parser.ParseComments)
		if perr != nil {
			t.Errorf("parse %s: %v", slash, perr)
			return nil
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if name == "TestMain" || !strings.HasPrefix(name, "Test") ||
				strings.HasPrefix(name, "TestArch_") || strings.HasPrefix(name, "TestMeta_") {
				continue
			}
			// Slice the source from the start of the docblock window
			// (~400 bytes preceding the func) through the end of the
			// func body. Any SQL-contract marker in that window counts.
			start := int(fd.Pos()) - 1
			windowStart := start - 400
			if windowStart < 0 {
				windowStart = 0
			}
			end := int(fd.End())
			if end > len(body) {
				end = len(body)
			}
			if perTestMarkerRE.MatchString(body[windowStart:end]) {
				continue
			}
			violations = append(violations, unmarkedTest{
				file: slash,
				fn:   name,
				line: fset.Position(fd.Pos()).Line,
			})
		}
		return nil
	})

	if len(violations) == 0 {
		return
	}

	slices.SortFunc(violations, func(a, b unmarkedTest) int {
		return cmp.Or(cmp.Compare(a.file, b.file), cmp.Compare(a.line, b.line))
	})

	t.Errorf("%d adapter integration test(s) without SQL-contract justification (ADR 0062):", len(violations))
	t.Logf("Every test in *_repository_pg_test.go / *_pg_integration_test.go MUST declare:")
	t.Logf("  (a) File-level: `// SQL-CONTRACT COVERAGE for this file ...` at the top, OR")
	t.Logf("  (b) Per-test:   `// SQL-contract: <what>` in or above the Test* func.")
	t.Logf("Reference shapes:")
	t.Logf("  internal/identity/adapters/role_repository_pg_test.go (file-level)")
	t.Logf("  internal/identity/adapters/tenant_repository_pg_test.go (per-test)")
	t.Logf("If the test isn't SQL-specific (round-trip, ErrNotFound, state machine),")
	t.Logf("delete it — covered by the aggregate's <aggregate>test.FakeRepository.")
	for _, v := range violations {
		t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
	}
}
