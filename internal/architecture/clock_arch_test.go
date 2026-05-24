// clock_arch_test.go — clock-injection invariants as a CI gate.
//
// Per the May 2026 clock-injection refactor (commit a33e9a0): the
// previous `internal/common/clock` package (with `clock.Now()`,
// `clock.Set()`, `clock.Reset()`, `freezeClock`, `activeFreezes`) is
// DELETED. Aggregates take `now time.Time` as an explicit parameter
// at the end of their method signatures. Handlers carry a
// `now func() time.Time` constructor dependency; composition root
// wires `time.Now`, tests inject a fixed-time closure.
//
// Canon: Khorikov §11 ("Mocking time"), Wild Workouts, Brandur
// "Postgres for everything" (deterministic ts in events), TDL Go-DDD.
//
// These three tests protect the refactor — any future re-introduction
// of a clock package or a time.Now() inside the domain trips a
// PR-time failure.

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 16: TestArch_NoClockPackageReference
// ----------------------------------------------------------------------------
//
// Enforces: the deleted `internal/common/clock` package stays deleted.
// Any production-code reference to `clock.Now`, `clock.Set`,
// `clock.Reset`, `freezeClock`, or `activeFreezes` is drift —
// either a partial-revert (someone re-added the package) or stale
// usage that escaped the refactor sweep.
//
// Detection: grep the file content excluding test files. Comments are
// permitted (the refactor's commit message and godoc legitimately
// reference the dead package by name) — so we tolerate matches that
// appear only inside `//` line comments or `/* */` block comments.
func TestArch_NoClockPackageReference(t *testing.T) {
	t.Parallel()

	tokens := []string{"clock.Now", "clock.Set", "clock.Reset", "freezeClock", "activeFreezes"}

	type violation struct {
		file  string
		token string
		line  int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		// Strip comments first — token references inside a comment are
		// fine (e.g. "replaces the prior clock.Now()").
		stripped := stripGoComments(string(src))
		for _, tok := range tokens {
			idx := 0
			for {
				j := strings.Index(stripped[idx:], tok)
				if j < 0 {
					break
				}
				absolute := idx + j
				line := 1 + strings.Count(stripped[:absolute], "\n")
				violations = append(violations, violation{
					file:  path,
					token: tok,
					line:  line,
				})
				idx = absolute + len(tok)
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("CLOCK-PACKAGE REVIVAL VIOLATIONS — %d", len(violations))
		t.Logf("The internal/common/clock package was deleted in May 2026")
		t.Logf("(commit a33e9a0). Any production-code reference to clock.Now,")
		t.Logf("clock.Set, clock.Reset, freezeClock, or activeFreezes means")
		t.Logf("either (a) a partial revert, or (b) a stale match that escaped")
		t.Logf("the refactor sweep. Use the injected `now func() time.Time`")
		t.Logf("on the handler or accept `now time.Time` as a parameter.")
		for _, v := range violations {
			t.Errorf("%s:%d — references %s (clock package is dead)", v.file, v.line, v.token)
		}
	}
}

// stripGoComments removes // line comments + /* */ block comments.
// We do a naive pass — sufficient for tests because our source is
// always go-fmt'd and never contains the comment delimiters inside
// string literals where they'd be load-bearing.
func stripGoComments(s string) string {
	// Replace block comments first.
	blockRE := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = blockRE.ReplaceAllStringFunc(s, func(m string) string {
		// Preserve newlines so line numbers stay correct.
		return strings.Repeat("\n", strings.Count(m, "\n"))
	})
	// Then line comments — preserve trailing newline.
	lineRE := regexp.MustCompile(`//[^\n]*`)
	s = lineRE.ReplaceAllString(s, "")
	return s
}

// ----------------------------------------------------------------------------
// Test 17: TestArch_NoTimeNowInDomain
// ----------------------------------------------------------------------------
//
// Enforces: domain layer is time-pure. Per Khorikov §11 + Wild
// Workouts + Vernon IDDD: time flows in via method parameters
// (`now time.Time` last arg). A direct `time.Now()` call in the
// domain makes the aggregate non-deterministic + un-testable
// without monkey-patching.
//
// Scope: every non-test Go file under internal/<module>/domain/.
// AST: walk CallExpr looking for `time.Now`.
func TestArch_NoTimeNowInDomain(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
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
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkg.Name == "time" && sel.Sel.Name == "Now" {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN-LAYER time.Now() VIOLATIONS — %d", len(violations))
		t.Logf("Per Khorikov §11 + Wild Workouts: domain is time-pure. Time")
		t.Logf("flows in via method parameters (`now time.Time` last arg).")
		t.Logf("Direct time.Now() calls make aggregates non-deterministic")
		t.Logf("and un-testable without monkey-patching.")
		for _, v := range violations {
			t.Errorf("%s:%d — time.Now() inside domain (use a `now time.Time` param)", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 18: TestArch_HandlersInjectNow
// ----------------------------------------------------------------------------
//
// Enforces: per the May 2026 clock-injection refactor — every
// `*Handler` method in `app/command/` or `app/query/` that needs
// wall-clock time MUST acquire it from an injected
// `now func() time.Time` field, NOT by calling `time.Now()` directly.
//
// Detection: AST walk every method on a *Handler receiver type in
// app/command/ + app/query/; flag any direct `time.Now()` call.
//
// This is the inverse formulation of "Handler must have a `now` field":
// instead of requiring the field on every handler (read-only handlers
// don't need it), we forbid time.Now() in handler methods. Handlers
// that touch wall-clock time WILL fail until they switch to h.now().
//
// The composition root (cmd/api / cmd/worker) wires `time.Now` into
// every handler ctor — so adopting this pattern is one struct-field
// + ctor-param + replace `time.Now()` with `h.now()` per handler.
func TestArch_HandlersInjectNow(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"command", "query"} {
			dir := filepath.Join(internalDir(t), mod, "app", sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || fd.Body == nil {
						continue
					}
					if !isHandlerReceiver(fd.Recv) {
						continue
					}
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := call.Fun.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						pkg, ok := sel.X.(*ast.Ident)
						if !ok {
							return true
						}
						if pkg.Name == "time" && sel.Sel.Name == "Now" {
							violations = append(violations, violation{
								file: path,
								line: fset.Position(call.Pos()).Line,
								fn:   fd.Name.Name,
							})
						}
						return true
					})
				}
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("HANDLER time.Now() VIOLATIONS — %d", len(violations))
		t.Logf("Per the May 2026 clock-injection refactor: handlers acquire")
		t.Logf("time via an injected `now func() time.Time` field. The")
		t.Logf("composition root wires `time.Now` everywhere; tests inject a")
		t.Logf("fixed-time closure for determinism. Direct time.Now() calls")
		t.Logf("inside handler methods break the test-determinism contract.")
		for _, v := range violations {
			t.Errorf("%s:%d — handler method %s calls time.Now() directly (use h.now())", v.file, v.line, v.fn)
		}
	}
}

// isHandlerReceiver reports whether a FuncDecl receiver names a
// type ending in "Handler" (e.g. *LoginHandler, ListSessionsHandler).
// Both pointer + value receivers are accepted.
func isHandlerReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return false
	}
	t := recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return false
	}
	return strings.HasSuffix(id.Name, "Handler")
}
