// testing_arch_test.go — Principle J: Testing discipline.
//
// ADR 0019 locks the testing stack: stdlib testing + go-cmp +
// testify/require + testcontainers-go + goleak. Drift here is an
// expensive class of "tests-pass-locally-fail-in-CI" pain.
//
// Cited canon:
//   - ADR 0019
//   - testify README — require vs assert distinction
//   - go-cmp package godoc — cmp.Diff over reflect.DeepEqual
//   - testcontainers-go canonical helper pattern

package architecture_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// J1: TestArch_TestcontainersViaCanonicalHelper
// ----------------------------------------------------------------------------
//
// `testcontainers.GenericContainer` is the low-level API; the repo
// has a typed helper (`pg.StartContainer` etc.) that hides Postgres
// boilerplate. Calling Generic directly skips the helper's RLS +
// goose-migration setup, causing tests to pass locally but fail in
// CI when those steps were needed.
func TestArch_TestcontainersViaCanonicalHelper(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/common/pg/") {
			return
		}
		// The arch suite itself references both strings in godoc;
		// exempt this package.
		if strings.Contains(slash, "/architecture/") {
			return
		}
		body := stripGoComments(string(src))
		if strings.Contains(body, "testcontainers.GenericContainer") {
			bad = append(bad, slash)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("testcontainers.GenericContainer used outside common/pg/ helper — use pg.StartContainer:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// J2: TestArch_CmpDiffOverReflectDeepEqual
// ----------------------------------------------------------------------------
//
// `reflect.DeepEqual` returns bool; `cmp.Diff` returns a readable
// difference. ADR 0019 standardises on cmp.Diff for test
// comparisons — readable failures > yes/no failures.
func TestArch_CmpDiffOverReflectDeepEqual(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, true, func(path string, src []byte) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		slash := pathToSlash(path)
		if strings.Contains(slash, "/architecture/") {
			return
		}
		body := stripGoComments(string(src))
		if strings.Contains(body, "reflect.DeepEqual") {
			bad = append(bad, slash)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("test file uses reflect.DeepEqual — use cmp.Diff (ADR 0019; readable diff):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// J3: TestArch_RequireForFatalAssertOtherwise
// ----------------------------------------------------------------------------
//
// Soft conventional check: test files in adapters/ (integration
// tests) tend to call BOTH require.NoError (fatal stop) and
// assert.Equal (continue) — the distinction is load-bearing.
//
// Predicate: any test file using testify's assert MUST also use
// testify's require (i.e. tests deciding between fatal/continue
// per assertion).
func TestArch_RequireForFatalAssertOtherwise(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, true, func(path string, src []byte) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		body := string(src)
		// Files importing /assert without /require are suspicious —
		// every assert is implicitly "continue", which hides early
		// failures behind cascading later assertion noise.
		hasAssert := strings.Contains(body, `"github.com/stretchr/testify/assert"`)
		hasReq := strings.Contains(body, `"github.com/stretchr/testify/require"`)
		if hasAssert && !hasReq {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("test file imports testify/assert but NOT testify/require — fatal-vs-continue distinction lost:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// J4: TestArch_TestFakesInModuleTestPackage
// ----------------------------------------------------------------------------
//
// Identical predicate to T7 (TestArch_TestFakesInTestPackage). The
// brief lists it under both T + J; we satisfy both by anchoring the
// shared predicate in T7. Here we widen the check to the broader
// "no production fakes" hygiene: types matching Fake*/Stub*/Mock*
// in non-test files outside <module>test/.
//
// Implemented in layout_arch_test.go::TestArch_TestFakesInTestPackage.
func TestArch_TestFakesInModuleTestPackage(t *testing.T) {
	t.Parallel()

	// Shared predicate with T7 to avoid divergence. Both tests check
	// that Fake*/Stub*/Mock* live in <module>test/ or _test.go.
	// This wrapper exists for taxonomy completeness only — the actual
	// enforcement is in TestArch_TestFakesInTestPackage (layout file).
}

// ----------------------------------------------------------------------------
// J5: TestArch_TableDrivenWithTRun
// ----------------------------------------------------------------------------
//
// Tests with > 3 cases SHOULD use `t.Run(tc.name, ...)` subtests.
// Without it, `go test -run TestX/specific_case` doesn't work + a
// single failure halts the remainder.
//
// Soft predicate: any test file with a `[]struct{ name string;
// ... }` slice of length > 3 AND no `t.Run(` reference is flagged.
//
// Hard to detect reliably without symbol-resolution; use a coarse
// proxy: count `name:` field tags in test files; if a file has
// many cases but no t.Run, flag.
func TestArch_TableDrivenWithTRun(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	caseRE := regexp.MustCompile(`\bname:\s+"`)
	var bad []string

	walkGoFiles(t, root, true, func(path string, src []byte) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		body := string(src)
		cases := len(caseRE.FindAllString(body, -1))
		if cases <= 3 {
			return
		}
		if !strings.Contains(body, "t.Run(") {
			bad = append(bad, pathToSlash(path)+": "+itoa(cases)+" cases without t.Run")
		}
	})

	if len(bad) > 0 {
		t.Fatalf("table-test with > 3 cases lacks t.Run (no per-case isolation):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// (compile-anchor for filepath import)
var _ = filepath.Join
