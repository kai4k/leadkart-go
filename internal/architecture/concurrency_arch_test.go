// concurrency_arch_test.go — Principle 8: Concurrency Safety.
//
// Per Bryan Mills "Rethinking Concurrency Patterns" (GopherCon 2018,
// still canon), Cheney "always pass ctx", and Go's race detector
// canon: concurrency is bounded, ctx-aware, and goroutine-leak-free.
// Request paths never spawn naked goroutines; channel buffers are
// bounded by const; integration tests use goleak.VerifyTestMain to
// detect leaks.
//
// Tests in this file:
//   48. TestArch_NoNakedGoroutinesInHandlers
//   49. TestArch_BoundedChannelBuffers
//   50. TestArch_GoleakInIntegrationTests
//   51. TestArch_NoTimeSleepInRequestPath

package architecture_test

import (
	"go/ast"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 48: TestArch_NoNakedGoroutinesInHandlers
// ----------------------------------------------------------------------------
//
// app/command/ and app/query/ files cannot contain `go ` statements
// (no fire-and-forget goroutines from request paths). Background work
// goes through the outbox + a subscriber, NOT a goroutine the request
// can't track.
func TestArch_NoNakedGoroutinesInHandlers(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"command", "query"} {
			dir := filepath.Join(internalDir(t), mod, "app", sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					gs, ok := n.(*ast.GoStmt)
					if !ok {
						return true
					}
					violations = append(violations, violation{
						file: path,
						line: fset.Position(gs.Pos()).Line,
					})
					return true
				})
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("NAKED GOROUTINE-IN-HANDLER VIOLATIONS — %d", len(violations))
		t.Logf("Per Bryan Mills GopherCon 2018: fire-and-forget goroutines")
		t.Logf("from request paths leak when the request cancels. Background")
		t.Logf("work goes through outbox + subscriber.")
		for _, v := range violations {
			t.Errorf("%s:%d — `go ...` inside command/query handler", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 49: TestArch_BoundedChannelBuffers
// ----------------------------------------------------------------------------
//
// `make(chan T, N)` where N is a numeric literal > 1024 OR a variable
// (not a const ≤ 1024) requires `// arch-test:bounded-justified`
// comment on the same line.
//
// Per Mills + sqlite-style ringbuffer pattern: unbounded buffers turn
// memory pressure into latency. Force every wide buffer to be
// justified inline.
func TestArch_BoundedChannelBuffers(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		got  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "make" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			// First arg must be a chan type.
			if _, ok := call.Args[0].(*ast.ChanType); !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			lineText := readLine(string(src), pos.Line)
			if hasArchTestDirective(lineText, "bounded-justified") {
				return true
			}
			// Buffer arg.
			bufArg := call.Args[1]
			lit, ok := bufArg.(*ast.BasicLit)
			if !ok {
				// Variable / call — count as violation requiring comment.
				violations = append(violations, violation{
					file: path,
					line: pos.Line,
					got:  "non-literal buffer (justify with // arch-test:bounded-justified)",
				})
				return true
			}
			if lit.Kind != token.INT {
				return true
			}
			bufSize, err := strconv.Atoi(lit.Value)
			if err != nil {
				return true
			}
			if bufSize > 1024 {
				violations = append(violations, violation{
					file: path,
					line: pos.Line,
					got:  "buffer = " + lit.Value,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("UNBOUNDED CHANNEL-BUFFER VIOLATIONS — %d", len(violations))
		t.Logf("Buffers > 1024 or non-literal sizes need an inline justification")
		t.Logf("comment `// arch-test:bounded-justified` so reviewers see the")
		t.Logf("memory-pressure rationale.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s", v.file, v.line, v.got)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 50: TestArch_GoleakInIntegrationTests
// ----------------------------------------------------------------------------
//
// Every package that ships *_integration_test.go files has a TestMain
// invoking goleak.VerifyTestMain (or imports a helper that wraps it).
// Per ADR 0019 + uber-go/goleak README: integration tests with
// testcontainers spawn long-lived goroutines (pgx pool, watermill
// subscriber) — a leak across test runs masks bugs the next test
// inherits.
func TestArch_GoleakInIntegrationTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		dir string
	}
	var violations []violation

	// Walk every dir under internal/ that contains at least one
	// *_integration_test.go.
	seenDirs := map[string]bool{}
	_ = filepath.WalkDir(internalDir(t), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "_integration_test.go") {
			return nil
		}
		seenDirs[filepath.Dir(path)] = true
		return nil
	})

	for dir := range seenDirs {
		hasGoleak := false
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				continue
			}
			text := string(raw)
			if strings.Contains(text, "goleak.VerifyTestMain") {
				hasGoleak = true
				break
			}
		}
		if !hasGoleak {
			violations = append(violations, violation{dir: dir})
		}
	}

	if len(violations) > 0 {
		t.Errorf("integration-test package does not wire goleak.VerifyTestMain — add testmain_integration_test.go (Uber goleak README + ADR 0019):")
		for _, v := range violations {
			t.Logf("  %s", v.dir)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 51: TestArch_NoTimeSleepInRequestPath
// ----------------------------------------------------------------------------
//
// app/command/, app/query/, ports/ cannot call time.Sleep. Use ctx-
// aware backoff (e.g. ticker + select on ctx.Done()) so cancellation
// short-circuits the wait.
func TestArch_NoTimeSleepInRequestPath(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"app", "ports"} {
			dir := filepath.Join(internalDir(t), mod, sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				ast.Inspect(f, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					pkg, name := callPkgAndName(call.Fun)
					if pkg == "time" && name == "Sleep" {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(call.Pos()).Line,
						})
					}
					return true
				})
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("time.Sleep IN REQUEST PATH VIOLATIONS — %d", len(violations))
		t.Logf("Use ctx-aware backoff (ticker + select on ctx.Done) so")
		t.Logf("cancellation short-circuits the wait.")
		for _, v := range violations {
			t.Errorf("%s:%d — time.Sleep in request path", v.file, v.line)
		}
	}
}
