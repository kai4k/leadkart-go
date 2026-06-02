// concurrency_arch_test.go — Principle 8: Concurrency Safety.
//
// Bryan Mills GopherCon 2018: concurrency is bounded, ctx-aware, goroutine-leak-free.
// No naked goroutines in request paths; channel buffers bounded; integration
// tests wire goleak.

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

// TestArch_NoNakedGoroutinesInHandlers asserts app/command/ and app/query/ have
// no `go` statements. Background work goes through outbox + subscriber.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_BoundedChannelBuffers asserts make(chan T, N) with N > 1024 or a
// non-literal carries arch-test:bounded-justified.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// TestArch_GoleakInIntegrationTests asserts every package with
// *_integration_test.go files has a TestMain using goleak.VerifyTestMain or
// goleak.Find. ADR 0019 + uber-go/goleak README.
func TestArch_GoleakInIntegrationTests(t *testing.T) {
	t.Parallel()

	type violation struct {
		dir string
	}
	var violations []violation

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
			if strings.Contains(text, "goleak.VerifyTestMain") ||
				strings.Contains(text, "goleak.Find") {
				hasGoleak = true
				break
			}
		}
		if !hasGoleak {
			violations = append(violations, violation{dir: dir})
		}
	}

	if len(violations) > 0 {
		t.Errorf("integration-test package does not wire goleak — add goleak.VerifyTestMain (or goleak.Find after a custom wrapper like pgtest.RunMain) per Uber goleak README + ADR 0019:")
		for _, v := range violations {
			t.Logf("  %s", v.dir)
		}
	}
}

// TestArch_NoTimeSleepInRequestPath asserts app/ and ports/ don't call
// time.Sleep. Use ctx-aware backoff instead.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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
