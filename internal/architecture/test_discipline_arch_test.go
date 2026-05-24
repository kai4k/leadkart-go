// test_discipline_arch_test.go — Principle TD: Unit + Integration test discipline.
//
// Companion catalog to testing_arch_test.go (Principle J, ADR 0019 stack
// choices). Where J pins the LIBRARIES (testify, go-cmp, testcontainers,
// goleak), TD pins the PATTERNS — how tests use those libraries.
//
// Cited Go-canon (NOT generic SRE):
//   - Dave Cheney "Prefer table-driven tests" (dave.cheney.net 2019)
//   - Mat Ryer "How I write HTTP services in Go after 13 years" §testing
//   - Russ Cox "Subtests and Sub-benchmarks" (Go blog 2016)
//   - Bryan Mills "Rethinking Concurrency Patterns" — t.Parallel everywhere
//   - Go 1.14 release notes — t.Cleanup
//   - Go 1.17 release notes — t.TempDir on test-end auto-cleanup
//   - Go 1.24 release notes — t.Context, t.Chdir, testing/synctest, T.Loop, B.Loop
//   - testify README — require vs assert distinction
//   - go-cmp package godoc — cmp.Diff over reflect.DeepEqual
//   - Uber goleak README — per-pkg TestMain wiring
//   - Brandur Leach "Crunchy Bridge testing infra" — testcontainers reuse
//   - Khorikov *Unit Testing* §8 — hidden inputs (time, randomness, network)
//
// Tests in this file (TD1-TD24):
//
//	TD1.  TestArch_TestFuncsCallTParallelOrCiteReason
//	TD2.  TestArch_SubtestsCallTParallelOrCiteReason
//	TD3.  TestArch_TestsUseTContext                       (Go 1.24+ canon)
//	TD4.  TestArch_NoOsMkdirTempInTests                   (use t.TempDir)
//	TD5.  TestArch_TestHelpersCallTHelper                 (helper-trace canon)
//	TD6.  TestArch_NoOsChdirInTests                       (use t.Chdir, Go 1.24+)
//	TD7.  TestArch_NoTimeNowInTests                       (tests pin time)
//	TD8.  TestArch_NoTimeSleepInTests                     (use synctest / ticker)
//	TD9.  TestArch_NoMathRandWithoutSeed                  (deterministic only)
//	TD10. TestArch_NoNetListenInTests                     (use httptest)
//	TD11. TestArch_NoOutboundHTTPInUnitTests              (no real http.Get/Post)
//	TD12. TestArch_NoPgxImportInUnitTests                 (DB only in integration)
//	TD13. TestArch_NoUnconditionalTSkipWithoutMarker
//	TD14. TestArch_NoTestRetryLoops                       (no for-sleep-assert)
//	TD15. TestArch_IntegrationTestsHaveTimeout
//	TD16. TestArch_IntegrationHTTPViaHttptest
//	TD17. TestArch_BenchmarksUseBLoop                     (Go 1.24+; over for i<b.N)
//	TD18. TestArch_PreferSynctestForGoroutineTiming
//	TD19. TestArch_TestsHaveAtLeastOneAssertion
//	TD20. TestArch_NoErrorIgnoredInTests                  (no _, _ = call())
//	TD21. TestArch_FixedNowVarsAreDateLiterals            (no time.Now-derived)
//	TD22. TestArch_RequireAfterFatalNotReached            (no t.Error after t.Fatal)
//	TD23. TestArch_NoFmtPrintlnInTests                    (use t.Logf)
//	TD24. TestArch_TestFilesPairWithProductionFile

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Shared filters
// ----------------------------------------------------------------------------

// archTestFile filters out the arch suite itself — many arch tests
// reference forbidden patterns in regex/string-literal form to DETECT
// them, which would self-flag.
func archTestFile(slash string) bool {
	return strings.Contains(slash, "/internal/architecture/")
}

// isTestFile is true for any *_test.go.
func isTestFile(slash string) bool {
	return strings.HasSuffix(slash, "_test.go")
}

// isIntegrationTestFile is true for files carrying the canonical
// `//go:build integration` build tag.
func isIntegrationTestFile(src []byte) bool {
	body := string(src)
	if len(body) > 4096 {
		body = body[:4096]
	}
	return strings.Contains(body, "//go:build integration")
}

// ----------------------------------------------------------------------------
// TD1: TestArch_TestFuncsCallTParallelOrCiteReason
// ----------------------------------------------------------------------------
//
// Per Bryan Mills + Cheney canon: tests run in parallel by default;
// serialized tests need a cited reason in the godoc. Catches the
// "forgot t.Parallel" drift that silently halves CI throughput.
//
// Predicate: every `func Test*(t *testing.T)` body either calls
// `t.Parallel()` directly OR has a godoc/comment line mentioning
// `serial:` or `// arch-test:serial` with rationale.
//
// Allow-list: TestMain (Go testing infra; can't be parallel),
// benchmarks (handled separately), helpers (caught by TD5).
func TestArch_TestFuncsCallTParallelOrCiteReason(t *testing.T) {
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
		fset, f := parseFile(t, path, src)
		body := string(src)
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
			// Look for t.Parallel() in the immediate body.
			hasParallel := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "t" || id.Name == "tt") && sel.Sel.Name == "Parallel" {
					hasParallel = true
					return false
				}
				return true
			})
			if hasParallel {
				continue
			}
			// Godoc-cited serialization escape hatch.
			start := int(fd.Pos()) - 1
			ctx := body[max0(start-400):start]
			if strings.Contains(ctx, "arch-test:serial") || strings.Contains(strings.ToLower(ctx), "serial:") {
				continue
			}
			violations = append(violations, violation{
				file: slash, fn: name, line: fset.Position(fd.Pos()).Line,
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test func(s) missing t.Parallel() — every Test* must call t.Parallel() OR carry an `arch-test:serial` godoc with rationale (Cheney, Mills; ADR 0019):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
		}
	}
}

// max0 — go 1.21's builtin max() returns nondeterministic types under
// type-inference here; tiny helper to keep the arch test free of
// import noise.
func max0(a int) int {
	if a < 0 {
		return 0
	}
	return a
}

// ----------------------------------------------------------------------------
// TD2: TestArch_SubtestsCallTParallelOrCiteReason
// ----------------------------------------------------------------------------
//
// Inside `t.Run("name", func(t *testing.T) { ... })`, the subtest
// closure body must call `t.Parallel()` (or pin the loop var per Go
// 1.22+ — irrelevant since 1.22; before then `tt := tc` was required).
//
// Catches the silent "parent parallel, children serial" pattern that
// negates the speedup. Allow-list via inline `// arch-test:serial`
// comment.
func TestArch_SubtestsCallTParallelOrCiteReason(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Run" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || (id.Name != "t" && id.Name != "b") {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			fn, ok := call.Args[1].(*ast.FuncLit)
			if !ok || fn.Body == nil {
				return true
			}
			// Search the closure body for t.Parallel().
			hasParallel := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if i, ok := s.X.(*ast.Ident); ok && i.Name == "t" && s.Sel.Name == "Parallel" {
					hasParallel = true
					return false
				}
				return true
			})
			if hasParallel {
				return true
			}
			// Allow-list: inline `// arch-test:serial` comment near the t.Run line.
			line := fset.Position(call.Pos()).Line
			lines := strings.Split(string(src), "\n")
			ctxStart := line - 3
			if ctxStart < 0 {
				ctxStart = 0
			}
			ctxEnd := line + 1
			if ctxEnd > len(lines) {
				ctxEnd = len(lines)
			}
			ctx := strings.Join(lines[ctxStart:ctxEnd], "\n")
			if strings.Contains(ctx, "arch-test:serial") {
				return true
			}
			violations = append(violations, violation{file: slash, line: line})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d t.Run subtest(s) missing t.Parallel() — sibling subtests must run in parallel OR carry inline `// arch-test:serial` comment with rationale (Russ Cox 'Subtests and Sub-benchmarks'; Cheney):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD3: TestArch_TestsUseTContext
// ----------------------------------------------------------------------------
//
// Go 1.24+ ships `(*testing.T).Context()` — auto-canceled when the
// test ends. Replaces `context.Background()` in test bodies (which
// risks leaking goroutines past test end + skips the canon
// "test-bound deadline" pattern).
//
// Allow-list: TestMain (no *testing.T in scope), files using
// `context.WithTimeout(context.Background(), ...)` to anchor a
// genuinely time-bounded deadline (still preferred to wrap t.Context).
func TestArch_TestsUseTContext(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	bgRE := regexp.MustCompile(`\bcontext\.Background\(\)`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		// Quick TestMain detection: if file is a testmain-only file
		// (only TestMain + setup helpers), allow context.Background()
		// — there's no *testing.T in scope.
		isTestMainOnly := strings.Contains(slash, "testmain_") ||
			strings.HasSuffix(slash, "/main_test.go")
		for i, ln := range strings.Split(text, "\n") {
			if !bgRE.MatchString(ln) {
				continue
			}
			if isTestMainOnly {
				continue
			}
			trimmed := strings.TrimSpace(ln)
			if strings.Contains(trimmed, "// arch-test:context-background") {
				continue
			}
			// Skip context.Background() inside context.WithTimeout(...) calls
			// — those anchor a genuinely time-bounded deadline.
			if strings.Contains(ln, "context.WithTimeout(context.Background()") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test file(s) use context.Background() — prefer t.Context() (Go 1.24+ canon; auto-canceled on test end). Wrap in context.WithTimeout when a deadline is needed. Annotate with `// arch-test:context-background` if intentional:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD4: TestArch_NoOsMkdirTempInTests
// ----------------------------------------------------------------------------
//
// `t.TempDir()` (Go 1.15+) auto-cleans at test end. `os.MkdirTemp`
// requires manual `defer os.RemoveAll` which is easy to forget on
// table-driven tests + skipped on `t.Fatal`.
func TestArch_NoOsMkdirTempInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if strings.Contains(ln, "os.MkdirTemp") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
			// ioutil.TempDir is the Go <1.16 ancestor — still occasionally
			// seen in copy-pasted snippets.
			if strings.Contains(ln, "ioutil.TempDir") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test file(s) use os.MkdirTemp / ioutil.TempDir — prefer t.TempDir() (Go 1.15+, auto-cleanup on test end):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD5: TestArch_TestHelpersCallTHelper
// ----------------------------------------------------------------------------
//
// Per Go testing docs §"Helpers" — any function that takes
// `*testing.T` (or `testing.TB`) and is NOT itself a Test* function
// must call `t.Helper()` so test failures report the CALLER's line
// number, not the helper's. Common bug: failing assertion inside a
// fixture builder reports the builder's t.Fatalf line, not the test's
// call site.
func TestArch_TestHelpersCallTHelper(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		fn   string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) && !strings.Contains(slash, "/test/") && !strings.Contains(slash, "test/") {
			return
		}
		if archTestFile(slash) {
			return
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Type.Params == nil {
				continue
			}
			name := fd.Name.Name
			// Skip Test* / Benchmark* / Example* / TestMain — those are
			// entry points, not helpers.
			if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Example") || strings.HasPrefix(name, "Fuzz") {
				continue
			}
			// First parameter must be *testing.T or testing.TB.
			if len(fd.Type.Params.List) == 0 {
				continue
			}
			pType := fd.Type.Params.List[0].Type
			star, ok := pType.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "testing" {
				continue
			}
			if sel.Sel.Name != "T" && sel.Sel.Name != "TB" && sel.Sel.Name != "B" && sel.Sel.Name != "F" {
				continue
			}
			// Look for t.Helper() in the body.
			hasHelper := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if i, ok := s.X.(*ast.Ident); ok && (i.Name == "t" || i.Name == "tb" || i.Name == "b" || i.Name == "f") && s.Sel.Name == "Helper" {
					hasHelper = true
					return false
				}
				return true
			})
			if hasHelper {
				continue
			}
			violations = append(violations, violation{
				file: slash, fn: name, line: fset.Position(fd.Pos()).Line,
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test-helper(s) missing t.Helper() — failures report caller line only when helpers mark themselves (Go testing docs §Helpers):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// TD6: TestArch_NoOsChdirInTests
// ----------------------------------------------------------------------------
//
// Go 1.24+ ships `(*testing.T).Chdir` — auto-restores the original
// working directory on test end. `os.Chdir` leaks the change across
// tests + breaks parallel runs (Chdir is process-global).
func TestArch_NoOsChdirInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if strings.Contains(ln, "os.Chdir(") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test file(s) use os.Chdir — prefer t.Chdir (Go 1.24+; auto-restores + safe with t.Parallel):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD7: TestArch_NoTimeNowInTests
// ----------------------------------------------------------------------------
//
// Per the project's clock-injection refactor (commit 4c6f4a1+) +
// Khorikov *Unit Testing* §8: tests must pin time via a fixed
// instant (`fixedNow := time.Date(...)`), NOT call `time.Now()`
// which is a hidden input that makes failures non-reproducible.
//
// Allow-list:
//   - TestArch_ files (architecture suite itself measures time)
//   - integration tests (some need wall-clock for testcontainers / DB
//     startup timing); they're flagged for review via TD15 instead
//   - inline `// arch-test:wall-clock` annotation
func TestArch_NoTimeNowInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if isIntegrationTestFile(src) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !strings.Contains(ln, "time.Now()") {
				continue
			}
			if strings.Contains(ln, "arch-test:wall-clock") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d unit-test line(s) call time.Now() — pin time via fixedNow / inject (clock-injection refactor; Khorikov 'Unit Testing' §8). Annotate with `// arch-test:wall-clock` if intentional:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD8: TestArch_NoTimeSleepInTests
// ----------------------------------------------------------------------------
//
// `time.Sleep` in tests is the canonical flake-introducer + slowest
// part of CI. Go 1.24+ ships `testing/synctest` for time-based
// concurrent tests (deterministic virtual clock). For asynchronous
// waits, use the `wait-until` pattern (ticker + ctx.Done()).
//
// Allow-list: inline `// arch-test:wait-justified` comment.
func TestArch_NoTimeSleepInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !strings.Contains(ln, "time.Sleep(") {
				continue
			}
			if strings.Contains(ln, "arch-test:wait-justified") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test line(s) call time.Sleep — prefer testing/synctest (Go 1.24+) or ticker + ctx.Done() wait-until pattern. Annotate with `// arch-test:wait-justified` if measured + necessary:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD9: TestArch_NoMathRandWithoutSeed
// ----------------------------------------------------------------------------
//
// `math/rand` and `math/rand/v2` (Go 1.22+) are pseudo-random — tests
// using them MUST seed explicitly so failures reproduce. Catch the
// classic "test passes 99 times, fails the 100th" anti-pattern.
//
// Allow-list:
//   - `rand.New(rand.NewSource(...))` is fine (explicit seed)
//   - `rand.NewChaCha8(...)` (Go 1.22+ v2 explicit seed)
//   - inline `// arch-test:non-deterministic` annotation
func TestArch_NoMathRandWithoutSeed(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// rand.IntN / rand.Float64 / rand.Shuffle at package level (not
	// `rand.New(...).IntN(...)`) is what we flag.
	pkgLevelRE := regexp.MustCompile(`\brand\.(Int|Float|Shuffle|Read|Perm|Uint)\w*\(`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !pkgLevelRE.MatchString(ln) {
				continue
			}
			if strings.Contains(ln, "arch-test:non-deterministic") {
				continue
			}
			// crypto/rand is OK (cryptographic; doesn't need seed).
			if strings.Contains(ln, "crypto/rand") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test line(s) call package-level math/rand without explicit seed — pin via rand.New(rand.NewSource(SEED)) or rand.NewChaCha8(SEED) (Go 1.22+ v2). Annotate `// arch-test:non-deterministic` if measured:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD10: TestArch_NoNetListenInTests
// ----------------------------------------------------------------------------
//
// `net.Listen` in tests races on port allocation, leaks listeners on
// failed tests, and bypasses the request-pipeline middleware chain.
// `httptest.NewServer` does all three correctly.
func TestArch_NoNetListenInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if strings.Contains(ln, "net.Listen(") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test line(s) use net.Listen — use httptest.NewServer (race-safe port + auto-cleanup):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD11: TestArch_NoOutboundHTTPInUnitTests
// ----------------------------------------------------------------------------
//
// Unit tests (build tag NOT `integration`) must not hit the real
// network. `http.Get`, `http.Post`, `http.DefaultClient` calls leak
// dependency on the outside world — flaky CI guaranteed.
//
// Use httptest.NewServer + Client() roundtripper, or an injected
// HTTP transport for upstream-call tests.
func TestArch_NoOutboundHTTPInUnitTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if isIntegrationTestFile(src) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if strings.Contains(ln, "http.Get(") ||
				strings.Contains(ln, "http.Post(") ||
				strings.Contains(ln, "http.PostForm(") ||
				strings.Contains(ln, "http.DefaultClient.") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d unit-test line(s) make outbound HTTP — use httptest.NewServer + Client() roundtripper:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD12: TestArch_NoPgxImportInUnitTests
// ----------------------------------------------------------------------------
//
// Unit tests (build tag NOT `integration`) must not import `pgx` /
// `pgxpool` / `pgtype`. Real-DB usage belongs in integration tests
// where testcontainers + goleak set the safety net.
//
// Allow-list:
//   - internal/common/pg/ — the canonical pgx home; tests cover the
//     pool helpers + QueryTracer hooks against the real package.
//   - internal/*/app/arch_test.go — module-local arch tests that scan
//     pgx imports as their SUBJECT (catching forbidden adapter leaks
//     up into app/).
//   - any file matching `*_repository_pg_test.go` (in-process adapter
//     unit tests that exercise pgx-typed fields via interfaces).
func TestArch_NoPgxImportInUnitTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		hit  string
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if isIntegrationTestFile(src) {
			return
		}
		// Allow-list: canonical pgx homes + module arch tests.
		if strings.Contains(slash, "/internal/common/pg/") {
			return
		}
		if strings.HasSuffix(slash, "/app/arch_test.go") {
			return
		}
		if strings.HasSuffix(slash, "_repository_pg_test.go") {
			return
		}
		text := string(src)
		bans := []string{
			`"github.com/jackc/pgx/v5"`,
			`"github.com/jackc/pgx/v5/pgxpool"`,
			`"github.com/jackc/pgx/v5/pgtype"`,
		}
		for _, b := range bans {
			if strings.Contains(text, b) {
				violations = append(violations, violation{file: slash, hit: b})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d unit-test file(s) import pgx/pgxpool — DB access belongs in *_integration_test.go (with build tag, goleak, testcontainers):", len(violations))
		for _, v := range violations {
			t.Logf("  %s — %s", v.file, v.hit)
		}
	}
}

// ----------------------------------------------------------------------------
// TD13: TestArch_NoUnconditionalTSkipWithoutMarker
// ----------------------------------------------------------------------------
//
// `t.Skip(...)` calls outside `if`/`switch` blocks indicate either
// (a) a known violation that should be tracked in KNOWN_VIOLATIONS.md,
// or (b) a test that should be deleted. Either way the call site needs
// a marker (`known violation:` / `arch-test:`).
func TestArch_NoUnconditionalTSkipWithoutMarker(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		// Also skip the meta arch test on the arch suite (handles its
		// own skips).
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !strings.Contains(ln, "t.Skip(") && !strings.Contains(ln, "t.Skipf(") {
				continue
			}
			// Allow conditional skips — t.Skip after an `if` on the prior
			// line is fine. Cheap heuristic: scan back 3 lines for `if `.
			start := i - 3
			if start < 0 {
				start = 0
			}
			lines := strings.Split(text, "\n")
			conditional := false
			for j := start; j < i; j++ {
				trim := strings.TrimSpace(lines[j])
				if strings.HasPrefix(trim, "if ") || strings.HasPrefix(trim, "switch ") {
					conditional = true
					break
				}
			}
			if conditional {
				continue
			}
			// Marker check — t.Skip(...) must mention `known violation:`
			// or `arch-test:` in the message string.
			if strings.Contains(ln, "known violation:") || strings.Contains(ln, "arch-test:") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d unconditional t.Skip(...) without `known violation:` / `arch-test:` marker — either delete the test or register the skip:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD14: TestArch_NoTestRetryLoops
// ----------------------------------------------------------------------------
//
// Retry loops in tests (`for { time.Sleep(); if check { break } }`)
// mask real timing bugs + add CI latency. Use synctest, ticker + ctx,
// or wait-on-channel pattern.
//
// Heuristic: a `for` loop body containing both `time.Sleep` AND a
// break/return/assertion call.
func TestArch_NoTestRetryLoops(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		fset, f := parseFile(t, path, src)
		lines := strings.Split(string(src), "\n")
		ast.Inspect(f, func(n ast.Node) bool {
			fs, ok := n.(*ast.ForStmt)
			if !ok {
				return true
			}
			if fs.Body == nil {
				return true
			}
			hasSleep := false
			hasBreak := false
			ast.Inspect(fs.Body, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && sel.Sel.Name == "Sleep" {
							hasSleep = true
						}
					}
				}
				if _, ok := inner.(*ast.BranchStmt); ok {
					hasBreak = true
				}
				return true
			})
			if !hasSleep || !hasBreak {
				return true
			}
			// Allow-list: `// arch-test:wait-justified` annotation on
			// the for-loop line or the 3 surrounding lines (godoc
			// directly above the loop).
			line := fset.Position(fs.Pos()).Line
			ctxStart := line - 3
			if ctxStart < 1 {
				ctxStart = 1
			}
			ctxEnd := line
			if ctxEnd > len(lines) {
				ctxEnd = len(lines)
			}
			ctx := strings.Join(lines[ctxStart-1:ctxEnd], "\n")
			if strings.Contains(ctx, "arch-test:wait-justified") {
				return true
			}
			violations = append(violations, violation{
				file: slash, line: line,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d test retry-loop(s) — replace `for { sleep; if check { break } }` with synctest (Go 1.24+) or ctx-aware wait-until. Annotate `// arch-test:wait-justified — <reason>` if measured + necessary:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD15: TestArch_IntegrationTestsHaveTimeout
// ----------------------------------------------------------------------------
//
// Every `*_integration_test.go` Test* function must wrap its work in
// a `context.WithTimeout(...)` (typically off `t.Context()`) so a
// hung testcontainers boot / stuck DB query fails the test fast +
// reports a real timeout rather than CI's 10-min wallclock-cap kill.
//
// Heuristic: file is integration-tagged AND contains at least one
// `context.WithTimeout` call OR uses a per-file `defaultIntegrationTimeout`
// constant.
func TestArch_IntegrationTestsHaveTimeout(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if !isIntegrationTestFile(src) {
			return
		}
		// TestMain files don't drive tests directly — they wire goleak;
		// timeout applies in the Test* funcs that they bracket.
		if strings.Contains(slash, "/testmain_integration_test.go") {
			return
		}
		text := string(src)
		if strings.Contains(text, "context.WithTimeout") ||
			strings.Contains(text, "context.WithDeadline") ||
			strings.Contains(text, "IntegrationTimeout") ||
			strings.Contains(text, "integrationTimeout") ||
			strings.Contains(text, "arch-test:no-timeout-needed") {
			return
		}
		violations = append(violations, violation{file: slash})
	})

	if len(violations) > 0 {
		t.Errorf("%d integration test file(s) without context.WithTimeout — every integration Test* must bound execution via t.Context() + WithTimeout. Annotate `// arch-test:no-timeout-needed` if not applicable:", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// TD16: TestArch_IntegrationHTTPViaHttptest
// ----------------------------------------------------------------------------
//
// HTTP-touching integration tests must wire the real mux via
// `httptest.NewServer(handler)` — bypassing makes the middleware
// chain untested (auth, recover, request log, rate limit).
//
// Heuristic: integration file references `http.NewRequest` or `http.Client`
// — must also reference `httptest.NewServer`.
func TestArch_IntegrationHTTPViaHttptest(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		if !isIntegrationTestFile(src) {
			return
		}
		text := string(src)
		hitsHTTP := strings.Contains(text, "http.NewRequest") ||
			strings.Contains(text, "http.Client{") ||
			strings.Contains(text, "http.DefaultClient")
		if !hitsHTTP {
			return
		}
		if strings.Contains(text, "httptest.NewServer") ||
			strings.Contains(text, "httptest.NewTLSServer") ||
			strings.Contains(text, "httptest.NewRecorder") ||
			strings.Contains(text, "arch-test:http-justified") {
			return
		}
		violations = append(violations, violation{file: slash})
	})

	if len(violations) > 0 {
		t.Errorf("%d integration test file(s) use raw http.Client without httptest — wire the real mux via httptest.NewServer to exercise the full middleware chain:", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// TD17: TestArch_BenchmarksUseBLoop
// ----------------------------------------------------------------------------
//
// Go 1.24+ ships `(*testing.B).Loop()` — the new canonical benchmark
// pattern. Replaces `for i := 0; i < b.N; i++ { ... }` which Russ Cox
// + the Go team have flagged as error-prone (forgets to reset timer
// after setup, accidentally optimised out by the compiler).
//
// Allow-list: legacy benchmark files until a focused sweep. For
// FORWARD-compat gate, fire only when MORE than ceiling-N legacy
// patterns appear (currently set to 0 — strict).
func TestArch_BenchmarksUseBLoop(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// Patterns: `for i := 0; i < b.N` / `for _ = range b.N` (the latter
	// is OK on 1.23+ but Loop() is preferred 1.24+).
	bnRE := regexp.MustCompile(`for\s+\w*\s*:?=?\s*0?\s*;?\s*\w*\s*<\s*b\.N`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !bnRE.MatchString(ln) {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d benchmark line(s) use legacy `for ... < b.N` — prefer b.Loop() (Go 1.24+; auto resets timer + foils compiler dead-code elim):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD18: TestArch_PreferSynctestForGoroutineTiming
// ----------------------------------------------------------------------------
//
// Tests that spawn goroutines AND wait on time-based events (Sleep,
// time.After, ticker) should use `testing/synctest` (Go 1.24+) for
// deterministic virtual time. Catches the common flake source:
// "goroutine fires at +50ms" measured against real wall-clock.
//
// Heuristic: file uses `go func()` AND `time.After` / `time.Tick` /
// `time.NewTimer` without importing `testing/synctest`.
func TestArch_PreferSynctestForGoroutineTiming(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		hasGoroutine := strings.Contains(text, "go func()") || strings.Contains(text, "go func(")
		hasTiming := strings.Contains(text, "time.After(") ||
			strings.Contains(text, "time.Tick(") ||
			strings.Contains(text, "time.NewTimer(") ||
			strings.Contains(text, "time.NewTicker(")
		if !hasGoroutine || !hasTiming {
			return
		}
		if strings.Contains(text, `"testing/synctest"`) ||
			strings.Contains(text, "arch-test:no-synctest") {
			return
		}
		violations = append(violations, violation{file: slash})
	})

	if len(violations) > 0 {
		t.Errorf("%d test file(s) spawn goroutines + use real time without testing/synctest — prefer synctest (Go 1.24+) for deterministic virtual time. Annotate `// arch-test:no-synctest` if measured + necessary:", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// TD19: TestArch_TestsHaveAtLeastOneAssertion
// ----------------------------------------------------------------------------
//
// A test that never asserts anything is dead code that passes silently
// even when the code under test breaks. Predicate: every Test* function
// body must contain at least one of: `require.X`, `assert.X`, `t.Error`,
// `t.Errorf`, `t.Fatal`, `t.Fatalf`, `t.Fail`, `cmp.Diff` followed by
// `t.X`.
//
// Allow-list: Test* funcs that ONLY delegate to a single helper call
// (the helper is presumed to assert; flagged elsewhere by TD5).
func TestArch_TestsHaveAtLeastOneAssertion(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		fn   string
		line int
	}
	var violations []violation

	// Assertion shape: testify/require + assert; t.Error* / t.Fatal*;
	// go-cmp diff; or any test-shaped helper whose name begins with
	// `wait` / `expect` / `assert` / `require` + opens a paren (the
	// project's canonical sync-helper pattern; e.g. `waitFor(t, ...)`
	// internally calls t.Fatal on timeout — TD5 already gates t.Helper
	// presence on those).
	assertionRE := regexp.MustCompile(`(require|assert)\.(\w+)\(|t\.(Error|Fatal|Fail)\w*\(|cmp\.Diff\(|\b(wait|expect|assert|require)[A-Z]\w*\(`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		fset, f := parseFile(t, path, src)
		body := string(src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if !strings.HasPrefix(name, "Test") || name == "TestMain" {
				continue
			}
			// Extract the function body source.
			startOff := int(fd.Body.Pos()) - 1
			endOff := int(fd.Body.End()) - 1
			if startOff < 0 || endOff > len(body) || startOff >= endOff {
				continue
			}
			fnBody := body[startOff:endOff]
			if assertionRE.MatchString(fnBody) {
				continue
			}
			// Allow: body is a single t.Run loop (subtests carry the
			// assertions — flagged separately if absent).
			if strings.Contains(fnBody, "t.Run(") {
				continue
			}
			violations = append(violations, violation{
				file: slash, fn: name, line: fset.Position(fd.Pos()).Line,
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test func(s) without any assertion — every Test* must call require.X / assert.X / t.Error* / t.Fatal* / cmp.Diff:", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// TD20: TestArch_NoErrorIgnoredInTests
// ----------------------------------------------------------------------------
//
// `_, _ = doSomething()` / `_ = repo.Add(ctx, x)` in test bodies
// silently swallows real failure modes. The test passes even though
// the operation broke. Use require.NoError or assign + assert.
//
// Allow-list:
//   - calls returning ONLY error where the test explicitly wants to
//     ignore (annotate `// arch-test:ignore-err`)
//   - `defer x.Close()` patterns (return value handled by defer)
func TestArch_NoErrorIgnoredInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// Patterns to flag — assignment to _ from an error-returning call.
	patternsRE := regexp.MustCompile(`^\s*_\s*=\s*\w+(\.\w+)*\(`)
	// Skip patterns that are conventional + safe per Go canon:
	//   - Close() / Terminate() / Shutdown() in cleanup paths
	//   - PullEvents (drain helper, deliberately ignores the slice)
	//   - Encode/Write on httptest recorders (response sink)
	// Match the method NAME anywhere on the line; arg list may vary
	// (e.g. `c.Terminate(ctx)` vs `c.Close()`).
	skipRE := regexp.MustCompile(`\.(Close|Terminate|Shutdown|Stop|Disconnect|Cleanup|Cancel|Release|Reset|PullEvents)\(|\.Write(Header)?\(|\.Encode\(`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if !patternsRE.MatchString(ln) {
				continue
			}
			if skipRE.MatchString(ln) {
				continue
			}
			if strings.Contains(ln, "arch-test:ignore-err") {
				continue
			}
			violations = append(violations, violation{file: slash, line: i + 1})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test line(s) ignore return value with `_ = call()` — handle the error explicitly via require.NoError. Annotate `// arch-test:ignore-err` if intentional:", len(violations))
		for _, v := range violations[:min0(len(violations), 30)] {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// min0 — companion to max0 (stdlib min would inflate imports).
func min0(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ----------------------------------------------------------------------------
// TD21: TestArch_FixedNowVarsAreDateLiterals
// ----------------------------------------------------------------------------
//
// Test files that declare a `fixedNow` / `testNow` variable must
// initialize it from a `time.Date(...)` literal — NOT from `time.Now()`
// or any clock-derived expression. The whole point of pinned time is
// reproducibility; deriving from Now() defeats it.
func TestArch_FixedNowVarsAreDateLiterals(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	nameRE := regexp.MustCompile(`(?m)^(var|\s+)\s*(fixedNow|testNow|nowFunc)\b[^=]*=`)
	dateRE := regexp.MustCompile(`time\.Date\(`)
	nowRE := regexp.MustCompile(`time\.Now\(\)`)

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		lines := strings.Split(text, "\n")
		for i, ln := range lines {
			if !nameRE.MatchString(ln) {
				continue
			}
			// Look at this line + the next 2 (multi-line decls).
			end := i + 3
			if end > len(lines) {
				end = len(lines)
			}
			window := strings.Join(lines[i:end], "\n")
			if dateRE.MatchString(window) {
				continue
			}
			if nowRE.MatchString(window) {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d fixedNow/testNow/nowFunc declaration(s) derived from time.Now() — must use time.Date literal (reproducibility):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD22 (REMOVED): unreachable-after-t.Fatal is detected by `go vet`'s
// built-in `unreachable` analyzer. A static AST-walk predicate at the
// arch-test layer cannot reliably distinguish the legitimate guard
// pattern `if err != nil { t.Fatalf(...) }` (canonical Go) from a true
// unreachable-code bug without duplicating go/types analysis. Leaving
// the check to go vet (which IS in `task ci`) avoids the false-positive
// burden + keeps the arch suite focused.

// ----------------------------------------------------------------------------
// TD23: TestArch_NoFmtPrintlnInTests
// ----------------------------------------------------------------------------
//
// `fmt.Println` / `fmt.Printf` writes to STDOUT bypassing the test
// framework's output capture. Use `t.Log` / `t.Logf` which routes
// through the test reporter (suppressed on pass; shown on fail).
//
// Sister rule to TestArch_NoFmtPrintInProduction (observability).
func TestArch_NoFmtPrintlnInTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			if strings.Contains(ln, "fmt.Println(") ||
				strings.Contains(ln, "fmt.Printf(") ||
				strings.Contains(ln, "fmt.Print(") {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test line(s) use fmt.Print* — prefer t.Log / t.Logf (routes through test reporter):", len(violations))
		for _, v := range violations {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TD24: TestArch_TestFilesPairWithProductionFile
// ----------------------------------------------------------------------------
//
// Every `<name>_test.go` (NOT _integration_test.go) under
// `internal/<mod>/{domain,app,ports,adapters}/` should have a paired
// `<name>.go` in the same dir. Catches orphan tests from deleted
// production files + tests-for-tests.
//
// Allow-list:
//   - helpers files (`helpers_test.go`, `*_helpers_test.go`)
//   - fakes files (`fakes_test.go`, `*_fakes_test.go`)
//   - testmain (`testmain*_test.go`)
//   - aggregate-private tests living in a sub-pkg test file
func TestArch_TestFilesPairWithProductionFile(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
	}
	var violations []violation

	skipSuffix := []string{
		"_integration_test.go",
		"helpers_test.go",
		"fakes_test.go",
		"main_test.go", // cmd/api/main_test.go etc.
	}
	skipPrefixBase := []string{
		"testmain",
		"flow_",     // multi-file flow tests
		"contract_", // contract test suites
		"e2e_",
	}

	root := filepath.Join(repoRoot(t), "internal")
	walkGoFiles(t, root, false, func(path string, _ []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) {
			return
		}
		base := filepath.Base(slash)
		for _, s := range skipSuffix {
			if strings.HasSuffix(base, s) {
				return
			}
		}
		for _, p := range skipPrefixBase {
			if strings.HasPrefix(base, p) {
				return
			}
		}
		// Determine paired production file.
		prodBase := strings.TrimSuffix(base, "_test.go") + ".go"
		prodPath := filepath.Join(filepath.Dir(path), prodBase)
		if _, err := readFileBytes(prodPath); err == nil {
			return
		}
		violations = append(violations, violation{file: slash})
	})

	if len(violations) > 0 {
		t.Errorf("%d orphan test file(s) without paired production file — delete the test or rename. Allow-list via filename prefix (testmain/flow/contract/e2e) or suffix (_helpers_test.go / _fakes_test.go):", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.file)
		}
	}
}
