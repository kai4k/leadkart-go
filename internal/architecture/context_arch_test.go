// context_arch_test.go — Principle F: Context discipline.
//
// Per Bryan Mills "Rethinking Concurrency Patterns" (GopherCon 2018,
// still canon) + the Go stdlib docs on context.Context: every
// goroutine in a request path must be cancellable; every blocking
// I/O call must accept ctx; ctx.Value keys must be typed; and the
// degenerate ctx escapes (Background / TODO) must stay confined to
// composition roots + tests.
//
// Cited canon:
//   - Bryan Mills — GopherCon 2018 (still canon)
//   - context package godoc
//   - Sameer Ajmani — "Go Concurrency Patterns: Context" (2014)
//   - Cheney — "Don't just check errors, handle them gracefully"

package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// F1: TestArch_NoContextBackgroundOutsideMainOrTest
// ----------------------------------------------------------------------------
//
// `context.Background()` returns a non-cancellable root context. The
// only legitimate sites are composition roots (cmd/*/main.go) +
// test files (where the test framework owns the lifecycle).
// Anywhere else, callers MUST receive ctx from upstream.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoContextBackgroundOutsideMainOrTest(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		// Subscribers can legitimately spawn child contexts at boot
		// time (their lifecycle is owned by the router), but the call
		// inside a handler is still flagged — caught by F3 / F5.
		body := stripGoComments(string(src))
		if strings.Contains(body, "context.Background()") {
			bad = append(bad, slash)
		}
	})

	// Cmd is not under internal/; not walked. Verify that.
	if len(bad) > 0 {
		t.Fatalf("context.Background() in production internal/ code — accept ctx from caller (Mills GopherCon 2018):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// F2: TestArch_NoContextTODOInProduction
// ----------------------------------------------------------------------------
//
// `context.TODO()` is for "I haven't decided yet" — a documentation
// signal that the call site needs revisit. Banned outside tests; if
// you need a temporary placeholder, accept ctx from upstream + plumb
// it through.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoContextTODOInProduction(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		if strings.Contains(body, "context.TODO()") {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("context.TODO() in production code (placeholder; plumb ctx instead):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// F3: TestArch_WithTimeoutHasDeferCancel
// ----------------------------------------------------------------------------
//
// `context.WithTimeout` / `WithCancel` / `WithDeadline` return a
// cancel fn. Failing to defer cancel() leaks a goroutine + a timer
// per call. Predicate: every `_, cancel := context.With*(...)` line
// must be followed within 3 lines by `defer cancel()`.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_WithTimeoutHasDeferCancel(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	declRE := regexp.MustCompile(`(\w+),\s*(\w+)\s*:=\s*context\.With(?:Timeout|Cancel|Deadline)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			m := declRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			cancelName := m[2]
			deferRE := regexp.MustCompile(`defer\s+` + regexp.QuoteMeta(cancelName) + `\s*\(\s*\)`)
			found := false
			for j := i; j < len(lines) && j < i+5; j++ {
				if deferRE.MatchString(lines[j]) {
					found = true
					break
				}
			}
			if !found {
				bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("context.With* missing defer cancel() within 5 lines (goroutine + timer leak):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// F4: TestArch_CtxValueKeysAreTyped
// ----------------------------------------------------------------------------
//
// `ctx.Value("foo")` with a string key collides silently across
// packages. context godoc: "The provided key must be comparable and
// should not be of type string or any other built-in type". Every
// ctx.Value() call must pass a typed key.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CtxValueKeysAreTyped(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Detect `ctx.Value("...")` (string literal first arg).
	stringKeyRE := regexp.MustCompile(`\bctx\.Value\(\s*"[^"]+"\s*\)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		if stringKeyRE.MatchString(body) {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("ctx.Value(\"raw string\") — keys MUST be typed package values (context godoc):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// F5: TestArch_CtxFirstParamInExportedFns
// ----------------------------------------------------------------------------
//
// Every exported function in `app/` and `adapters/` (the request
// path) must take `ctx context.Context` as its first parameter.
// This is the strongest single discipline for cancel-propagation +
// trace-context propagation through the codebase. Stdlib net/http +
// pgx canon.
//
// Allow-list: constructors (`New*`) that take no ctx are fine;
// boolean predicates (`IsX()`) that don't do I/O are fine. The test
// flags only fns whose body contains I/O markers (Query/Exec/HTTP
// roundtrip/etc.).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CtxFirstParamInExportedFns(t *testing.T) {
	t.Parallel()

	ioMarkers := []string{
		".Query(", ".QueryRow(", ".Exec(", ".QueryContext(",
		".ExecContext(", ".Do(", "http.NewRequest", "http.Get",
		"http.Post",
	}

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"app", "adapters"} {
			dir := filepath.Join(root, mod, layer)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				slash := pathToSlash(path)
				if strings.Contains(slash, "/adapters/db/") {
					return // generated sqlc
				}
				fset, file := parseFile(t, path, src)
				ast.Inspect(file, func(n ast.Node) bool {
					fd, ok := n.(*ast.FuncDecl)
					if !ok || !fd.Name.IsExported() {
						return true
					}
					// Constructors are exempt.
					if strings.HasPrefix(fd.Name.Name, "New") {
						return true
					}
					// Check whether the body does I/O.
					var body string
					if fd.Body != nil {
						start := fset.Position(fd.Body.Pos()).Offset
						end := fset.Position(fd.Body.End()).Offset
						if end > len(src) {
							end = len(src)
						}
						if start >= 0 && start < end {
							body = string(src[start:end])
						}
					}
					doesIO := false
					for _, mk := range ioMarkers {
						if strings.Contains(body, mk) {
							doesIO = true
							break
						}
					}
					if !doesIO {
						return true
					}
					if fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
						bad = append(bad, slash+": "+fd.Name.Name+" has no ctx param")
						return true
					}
					first := fd.Type.Params.List[0]
					typeStr := exprString(first.Type)
					if typeStr != "context.Context" {
						bad = append(bad, slash+": "+fd.Name.Name+" first param is "+typeStr)
					}
					return true
				})
			})
		}
	}

	if len(bad) > 0 {
		t.Fatalf("exported I/O-doing fn doesn't take ctx as first param (pgx/HTTP canon):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// F6: TestArch_GoroutinesInheritCtx
// ----------------------------------------------------------------------------
//
// Spawned goroutines MUST receive the parent ctx (or a derived ctx)
// — otherwise cancellation never reaches the worker. Heuristic: any
// `go func(...)` whose body contains an I/O call must be passed
// `ctx` (or `c context.Context`) as a parameter, OR the goroutine
// must close over `ctx` from the enclosing scope.
//
// This is a soft check (heuristic) — false positives opt out via
// `// arch-test:goroutine-detached <reason>` comment.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_GoroutinesInheritCtx(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		modDir := filepath.Join(root, mod)
		walkGoFiles(t, modDir, false, func(path string, src []byte) {
			lines := strings.Split(string(src), "\n")
			for i, ln := range lines {
				if !strings.Contains(ln, "go func(") {
					continue
				}
				if strings.Contains(ln, "arch-test:goroutine-detached") {
					continue
				}
				// Heuristic: scan the next 15 lines for any I/O marker
				// AND for any reference to ctx.
				hasIO := false
				hasCtx := false
				for j := i; j < len(lines) && j < i+20; j++ {
					l := lines[j]
					if strings.Contains(l, ".Exec(") || strings.Contains(l, ".Query(") ||
						strings.Contains(l, "http.") {
						hasIO = true
					}
					if strings.Contains(l, "ctx") {
						hasCtx = true
					}
				}
				if hasIO && !hasCtx {
					bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("goroutine does I/O without ctx in scope (cancellation can't reach worker):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// exprString flattens an AST expression to its source-like textual
// form. Used by F5 to render parameter types in error messages.
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	}
	return "<complex>"
}

// Ensure go/token is imported (used for FileSet position math in F5).
var _ token.Pos = 0
