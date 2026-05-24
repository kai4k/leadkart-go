// modern_go_arch_test.go — Go 1.26+ idioms + banned-dependency gates.
//
// Per ADR 0034 (Go 1.26+), ADR 0013 (log/slog stdlib), CLAUDE.md
// "Banned" list, and Stripe / Square / PayPal canon on money handling.

package architecture_test

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 11: TestArch_NoInterfaceEmpty
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0034 (Go 1.26+) — `interface{}` is forbidden in
// hand-written code. Use `any` (Go 1.18+ alias). Generated files
// (sqlc, mocks, oapi-codegen) are exempt — `walkGoFiles` skips them
// via the canonical "Code generated ... DO NOT EDIT" marker.
//
// EXCEPTION: lines carrying a `// nolint:` directive (the file's
// author has explicitly opted out + accepted ownership of the
// violation).
func TestArch_NoInterfaceEmpty(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		text string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			it, ok := n.(*ast.InterfaceType)
			if !ok {
				return true
			}
			if it.Methods != nil && len(it.Methods.List) > 0 {
				return true
			}
			pos := fset.Position(it.Pos())
			// Read the line; skip if it contains a nolint directive.
			lineText := readLine(string(src), pos.Line)
			if strings.Contains(lineText, "// nolint:") || strings.Contains(lineText, "//nolint:") {
				return true
			}
			violations = append(violations, violation{
				file: path,
				line: pos.Line,
				text: strings.TrimSpace(lineText),
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("interface{} VIOLATIONS — %d (use `any` per ADR 0034)", len(violations))
		t.Logf("Go 1.18 introduced `any` as an alias for interface{}. ADR 0034")
		t.Logf("pins Go 1.26+; the modern spelling is `any`. Add a `// nolint:`")
		t.Logf("comment on the line if the literal interface{} is mandatory")
		t.Logf("(e.g. matching a generated signature byte-for-byte).")
		for _, v := range violations {
			t.Errorf("%s:%d — %s", v.file, v.line, v.text)
		}
	}
}

// readLine returns the 1-indexed Nth line of src, or "" if out of range.
func readLine(src string, n int) string {
	if n < 1 {
		return ""
	}
	cur := 1
	start := 0
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			if cur == n {
				return src[start:i]
			}
			cur++
			start = i + 1
		}
	}
	if cur == n {
		return src[start:]
	}
	return ""
}

// ----------------------------------------------------------------------------
// Test 12: TestArch_NoLogPackage
// ----------------------------------------------------------------------------
//
// Enforces: ADR 0013 — structured logging via `log/slog` (stdlib).
// The bare `log` package emits unstructured text + has no levels +
// no context propagation; using it bypasses every observability
// invariant the project ships (correlation IDs, tenant context,
// OTel span linkage). No exceptions.
func TestArch_NoLogPackage(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), true, func(path string, src []byte) {
		for _, imp := range parseImports(t, path, src) {
			if imp == "log" {
				violations = append(violations, violation{file: path})
			}
		}
	})

	if len(violations) > 0 {
		t.Logf(`PLAIN "log" IMPORT VIOLATIONS — %d (use "log/slog" per ADR 0013)`, len(violations))
		t.Logf(`The bare "log" package emits unstructured text + has no levels.`)
		t.Logf(`LeadKart-Go pins log/slog as the sole logger (ADR 0013).`)
		for _, v := range violations {
			t.Errorf("%s — imports plain \"log\"", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 13: TestArch_NoBannedDeps
// ----------------------------------------------------------------------------
//
// Enforces: CLAUDE.md "Banned" list + ADR 0004 (sqlc/pgx) + ADR 0013
// (log/slog). Parses go.mod and rejects DIRECT require statements for
// any dependency on the banned list. Indirect transitives are
// tolerated — they ride in via legitimate direct deps (e.g.
// `sirupsen/logrus` is pulled in transitively by testcontainers-go
// but no LeadKart code imports it directly).
//
// Detection: scan go.mod for direct require entries (not indirect).
func TestArch_NoBannedDeps(t *testing.T) {
	t.Parallel()

	banned := map[string]string{
		"gorm.io":                     "GORM is banned per ADR 0004 (sqlc + pgx is the canon)",
		"entgo.io":                    "Ent is banned per ADR 0004 (rejected after deep validation; see plan §G.D)",
		"bob.io":                      "bob is banned per ADR 0004",
		"github.com/gorilla/websocket": "gorilla/websocket is banned per ADR 0016 (coder/websocket is the canon)",
		"go.uber.org/zap":             "zap is banned per ADR 0013 (log/slog stdlib only)",
		"github.com/sirupsen/logrus":  "logrus is banned per ADR 0013 — DIRECT imports forbidden (indirect via testcontainers-go is tolerated)",
		"github.com/Masterminds/sprig": "sprig is banned (templating bloat)",
	}

	goMod := filepath.Join(repoRoot(t), "go.mod")
	raw, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	// Parse go.mod lines. A direct require has form:
	//   <path> <version>
	// inside a `require (` block, OR
	//   require <path> <version>
	// Indirect requires carry a trailing `// indirect` comment.
	type violation struct {
		dep    string
		reason string
	}
	var violations []violation

	inRequireBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "//") || trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "require (") {
			inRequireBlock = true
			continue
		}
		if inRequireBlock && trim == ")" {
			inRequireBlock = false
			continue
		}

		var depLine string
		switch {
		case inRequireBlock:
			depLine = trim
		case strings.HasPrefix(trim, "require "):
			depLine = strings.TrimSpace(strings.TrimPrefix(trim, "require"))
		default:
			continue
		}

		// Skip indirects.
		if strings.Contains(depLine, "// indirect") {
			continue
		}
		// Split into path + version (+ optional comment).
		fields := strings.Fields(depLine)
		if len(fields) < 1 {
			continue
		}
		dep := fields[0]
		for prefix, reason := range banned {
			if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
				violations = append(violations, violation{dep: dep, reason: reason})
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("BANNED DIRECT DEPENDENCY VIOLATIONS — %d", len(violations))
		t.Logf("Per CLAUDE.md `Banned` list + ADR 0004/0013/0016: these")
		t.Logf("packages MUST NOT appear as direct requires in go.mod.")
		for _, v := range violations {
			t.Errorf("%s — %s", v.dep, v.reason)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 14: TestArch_NoMustInRequestPath
// ----------------------------------------------------------------------------
//
// Enforces: CLAUDE.md ctor-patterns — `MustNewX` is an ANTI-PATTERN in
// request paths. Limited to constructor-like names (Must + Verb where
// Verb ∈ {New, Parse, Compile, Load, Init, Build, Open, Create,
// Configure}) to avoid false positives on legitimate domain methods
// like `MustChangePassword()` (a boolean getter on Person).
//
// Scope: non-test files under internal/<module>/{app,ports,adapters}/.
// (domain/ already does its own validation via factories returning err.)
//
// EXCEPTIONS:
//   - `init()` functions — init-time panics are tolerated.
//   - Package-level `var x = pkg.MustX(...)` — init-time too.
func TestArch_NoMustInRequestPath(t *testing.T) {
	t.Parallel()

	// constructorMustRE matches `MustNew`, `MustParse`, `MustCompile`,
	// `MustLoad`, `MustInit`, `MustBuild`, `MustOpen`, `MustCreate`,
	// `MustConfigure`. Allows trailing PascalCase to match concrete
	// ctor names like `MustNewIssuer`.
	constructorMustRE := regexp.MustCompile(`^Must(New|Parse|Compile|Load|Init|Build|Open|Create|Configure)([A-Z]\w*)?$`)

	type violation struct {
		file string
		line int
		name string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"app", "ports", "adapters"} {
			layerDir := filepath.Join(internalDir(t), mod, layer)
			walkGoFiles(t, layerDir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				// For each FuncDecl that is NOT an init function,
				// walk its body for forbidden Must* call expressions.
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Body == nil {
						continue
					}
					if fd.Name.Name == "init" {
						continue
					}
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						name := callName(call.Fun)
						if name == "" {
							return true
						}
						if !constructorMustRE.MatchString(name) {
							return true
						}
						violations = append(violations, violation{
							file: path,
							line: fset.Position(call.Pos()).Line,
							name: name,
						})
						return true
					})
				}
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("Must* IN REQUEST PATH VIOLATIONS — %d", len(violations))
		t.Logf("Per CLAUDE.md ctor-patterns: panicking constructors are")
		t.Logf("init-time + tests ONLY. Move package-level inits to")
		t.Logf("`var x = pkg.MustX(...)` (at package scope) or replace")
		t.Logf("with the (T, error) variant + bubble the error.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s called inside a function body", v.file, v.line, v.name)
		}
	}
}

// callName returns the trailing identifier of a function-call
// expression, supporting `pkg.Func`, `recv.Method`, and bare `Func`.
func callName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// ----------------------------------------------------------------------------
// Test 15: TestArch_NoFloat64ForMoney
// ----------------------------------------------------------------------------
//
// Enforces: Stripe canon — money is stored + computed as `int64` in
// the SMALLEST unit (paise for INR; cents for USD). Floating-point
// representation loses precision on cents-level arithmetic + leads to
// the classic "$0.30 - $0.10 - $0.10 - $0.10 != 0" bug. Per Square /
// PayPal / Adyen guides: never represent monetary amounts as float.
//
// Detection: AST walk every FieldDecl / GenDecl; if the field/var
// name matches the money-related regex AND the type is float32/64,
// fail. Case-insensitive name match.
//
// Scope: every non-generated non-test Go file under internal/.
func TestArch_NoFloat64ForMoney(t *testing.T) {
	t.Parallel()

	// Money-related identifiers (case-insensitive).
	moneyRE := regexp.MustCompile(`(?i)(^|_)(money|paise|amount|price|cost|balance|fee|payment|charge|refund|debit|credit|fare|tax|gst_amount)($|_)`)

	isFloatType := func(t ast.Expr) bool {
		id, ok := t.(*ast.Ident)
		if !ok {
			return false
		}
		return id.Name == "float32" || id.Name == "float64"
	}

	type violation struct {
		file string
		line int
		name string
		typ  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			// Struct field declarations.
			if st, ok := n.(*ast.StructType); ok && st.Fields != nil {
				for _, field := range st.Fields.List {
					if !isFloatType(field.Type) {
						continue
					}
					for _, name := range field.Names {
						if moneyRE.MatchString(name.Name) {
							violations = append(violations, violation{
								file: path,
								line: fset.Position(field.Pos()).Line,
								name: name.Name,
								typ:  typeName(field.Type),
							})
						}
					}
				}
			}
			// Var / Const declarations.
			if gd, ok := n.(*ast.GenDecl); ok && (gd.Tok == token.VAR || gd.Tok == token.CONST) {
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || vs.Type == nil {
						continue
					}
					if !isFloatType(vs.Type) {
						continue
					}
					for _, name := range vs.Names {
						if moneyRE.MatchString(name.Name) {
							violations = append(violations, violation{
								file: path,
								line: fset.Position(name.Pos()).Line,
								name: name.Name,
								typ:  typeName(vs.Type),
							})
						}
					}
				}
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("FLOAT-FOR-MONEY VIOLATIONS — %d (CRITICAL — Stripe canon: int64 in smallest unit)", len(violations))
		t.Logf("Floating-point can't represent decimal money exactly.")
		t.Logf("Store + compute as int64 paise (INR) / cents (USD).")
		t.Logf("See: Stripe API docs `amount`, Square `BigDecimal`, PayPal")
		t.Logf("`Money` schema — all 64-bit integer in smallest unit.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s %s (money field/var must not be float)", v.file, v.line, v.name, v.typ)
		}
	}
}

// typeName returns the textual representation of an Ident-typed
// expression (for use in error messages).
func typeName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<complex-type>"
}
