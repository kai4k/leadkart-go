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
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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
		"gorm.io":                      "GORM is banned per ADR 0004 (sqlc + pgx is the canon)",
		"entgo.io":                     "Ent is banned per ADR 0004 (rejected after deep validation; see plan §G.D)",
		"bob.io":                       "bob is banned per ADR 0004",
		"github.com/gorilla/websocket": "gorilla/websocket is banned per ADR 0016 (coder/websocket is the canon)",
		"go.uber.org/zap":              "zap is banned per ADR 0013 (log/slog stdlib only)",
		"github.com/sirupsen/logrus":   "logrus is banned per ADR 0013 — DIRECT imports forbidden (indirect via testcontainers-go is tolerated)",
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
//
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
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

// ----------------------------------------------------------------------------
// Test 56: TestArch_OmitzeroNotOmitempty
// ----------------------------------------------------------------------------
//
// Per Go 1.24+ idiom: JSON struct tags on slice/map/pointer fields use
// `omitzero` (added in Go 1.24) NOT `omitempty`. The historical
// `omitempty` semantics are bizarre for slices/maps (only nil omits;
// empty literal doesn't) and a foot-gun for callers.
//
// EXCEPTION: time.Time fields can use either — omitempty special-cases
// the zero time properly; pre-1.24 codebases still use that idiom.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_OmitzeroNotOmitempty(t *testing.T) {
	t.Parallel()

	// Match `json:"...,omitempty"` on slice/map/pointer fields.
	jsonTagRE := regexp.MustCompile(`json:"[^"]*,omitempty"`)

	type violation struct {
		file string
		line int
		fld  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				if !jsonTagRE.MatchString(field.Tag.Value) {
					continue
				}
				// Allow time.Time / *time.Time fields.
				if typeIsTime(field.Type) {
					continue
				}
				// Only flag for slice/map/pointer types.
				isCollection := false
				switch field.Type.(type) {
				case *ast.ArrayType, *ast.MapType, *ast.StarExpr:
					isCollection = true
				}
				if !isCollection {
					continue
				}
				for _, fn := range field.Names {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(field.Pos()).Line,
						fld:  fn.Name,
					})
				}
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("omitempty-ON-COLLECTION VIOLATIONS — %d", len(violations))
		t.Logf("Per Go 1.24+: slice/map/pointer fields use `omitzero` —")
		t.Logf("`omitempty` only omits nil, not empty literal.")
		for _, v := range violations {
			t.Errorf("%s:%d — field %s uses ,omitempty (use ,omitzero)", v.file, v.line, v.fld)
		}
	}
}

// typeIsTime reports whether a type expr names time.Time or *time.Time.
func typeIsTime(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "time" && sel.Sel.Name == "Time"
}

// ----------------------------------------------------------------------------
// Test 57: TestArch_ModernForRange
// ----------------------------------------------------------------------------
//
// Per Go 1.22+: `for i := 0; i < N; i++` where N is a constant int
// literal should be `for i := range N`. The new shape is shorter +
// emphasises "iterate N times, no condition gymnastics".
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_ModernForRange(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			fs, ok := n.(*ast.ForStmt)
			if !ok || fs.Init == nil || fs.Cond == nil || fs.Post == nil {
				return true
			}
			// Init: `i := 0`.
			as, ok := fs.Init.(*ast.AssignStmt)
			if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			litZero, ok := as.Rhs[0].(*ast.BasicLit)
			if !ok || litZero.Kind != token.INT || litZero.Value != "0" {
				return true
			}
			// Cond: `i < <int-literal>`.
			be, ok := fs.Cond.(*ast.BinaryExpr)
			if !ok || be.Op != token.LSS {
				return true
			}
			litN, ok := be.Y.(*ast.BasicLit)
			if !ok || litN.Kind != token.INT {
				return true
			}
			// Post: `i++`.
			inc, ok := fs.Post.(*ast.IncDecStmt)
			if !ok || inc.Tok != token.INC {
				return true
			}
			violations = append(violations, violation{
				file: path,
				line: fset.Position(fs.Pos()).Line,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("LEGACY for-i-loop VIOLATIONS — %d", len(violations))
		t.Logf("Per Go 1.22+: `for i := 0; i < N; i++` with N a constant")
		t.Logf("integer literal should be `for i := range N`.")
		for _, v := range violations {
			t.Errorf("%s:%d — `for i := 0; i < <lit>; i++` (use `for i := range <lit>`)", v.file, v.line)
		}
	}
}

// ============================================================================
// Principle X — Pure-domain channel-shape (1 test added per the comprehensive
// catalog brief).
// ============================================================================

// ----------------------------------------------------------------------------
// X1: TestArch_NoMakeChannelWithoutSize
// ----------------------------------------------------------------------------
//
// `make(chan T)` (unbuffered) blocks the sender until a receiver is
// ready — that's a coordination tool, not a data-flow tool. Drift
// signal: an accidental unbuffered chan in a producer/consumer
// shape silently turns the producer into the consumer's
// latency-buddy.
//
// Allow-list: explicit `// arch-test:signal-channel <reason>` for
// the legitimate "wake-up signal" pattern.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoMakeChannelWithoutSize(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	unbufRE := regexp.MustCompile(`make\(\s*chan\s+\w+\s*\)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			if !unbufRE.MatchString(ln) {
				continue
			}
			if strings.Contains(ln, "arch-test:signal-channel") {
				continue
			}
			// Check the prior line for the marker too.
			if i > 0 && strings.Contains(lines[i-1], "arch-test:signal-channel") {
				continue
			}
			bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("make(chan T) without buffer size (unbuffered = coordination, not flow). Opt out: arch-test:signal-channel:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ============================================================================
// Principles Q + R — Numeric precision + Timezone discipline (5 tests added).
// Q catches money-as-int64 violations; R catches accidental local-tz reads.
// ============================================================================

// ----------------------------------------------------------------------------
// Q1: TestArch_PercentagesAsBasisPoints
// ----------------------------------------------------------------------------
//
// Percentage fields (e.g. tax_rate) MUST be expressed as basis
// points (integer; 1bp = 0.01%). Stripe canon: floats in money
// shapes are silent rounding bugs.
//
// Predicate: struct fields with names ending `_rate`, `_pct`,
// `_percent` flagged unless renamed to `_bps` AND typed as an int
// kind. Soft (opt-out via marker).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PercentagesAsBasisPoints(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		body := stripGoComments(string(src))
		// Find struct-field decls with the offending suffix.
		fieldRE := regexp.MustCompile(`(\w+(?:_rate|_pct|_percent))\s+(\w+)`)
		for _, m := range fieldRE.FindAllStringSubmatch(body, -1) {
			// Allow if explicitly opt-out.
			if strings.Contains(body, "arch-test:non-bps-percentage") {
				continue
			}
			bad = append(bad, slash+": "+m[1]+" "+m[2]+" (use *_bps + int)")
		}
	})

	if len(bad) > 0 {
		t.Fatalf("percentage field doesn't use *_bps (basis-points) naming (Stripe canon):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// Q2: TestArch_NoFloatInPersistedDecimals
// ----------------------------------------------------------------------------
//
// float32 / float64 in struct fields tagged for DB persistence is
// a known rounding-bug source for money. Use int64 (smallest unit)
// or pgtype.Numeric.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoFloatInPersistedDecimals(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		body := stripGoComments(string(src))
		// Field decl like `Amount float64 \`db:"amount"\`` — look for
		// `float32` / `float64` followed by a backtick `db:"`.
		floatDBRE := regexp.MustCompile(`(\w+)\s+(?:float32|float64)\s+\x60[^\x60]*db:"`)
		for _, m := range floatDBRE.FindAllStringSubmatch(body, -1) {
			bad = append(bad, slash+": "+m[1])
		}
	})

	if len(bad) > 0 {
		t.Fatalf("float field tagged for DB persistence — use int64 (smallest unit) or pgtype.Numeric:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// R1: TestArch_NoTimeLocalInBusiness
// ----------------------------------------------------------------------------
//
// `time.Local` / `time.LoadLocation` outside UTC-formatting helpers
// is an accidental-local-timezone hazard. LeadKart serves Indian
// pharma but stores UTC end-to-end (BRD canon).
//
// Allow-list: cmd/ binaries that legitimately format for user
// display.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoTimeLocalInBusiness(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		// `time.LoadLocation` is legitimate for VALIDATION (testing
		// that a user-supplied IANA tz string is parseable). We flag
		// only assignments / variable captures from it: `loc :=
		// time.LoadLocation(...)` followed by a use that ISN'T `_`.
		if strings.Contains(body, "time.Local") {
			bad = append(bad, pathToSlash(path)+": time.Local")
		}
		loadRE := regexp.MustCompile(`(\w+),\s*err\s*:?=\s*time\.LoadLocation\(`)
		for _, m := range loadRE.FindAllStringSubmatch(body, -1) {
			if m[1] != "_" {
				bad = append(bad, pathToSlash(path)+": captures time.LoadLocation result into "+m[1])
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("time.Local / captured time.LoadLocation in business code — UTC end-to-end (BRD canon):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// R2: TestArch_TimeParsedAsUTCAtBoundary
// ----------------------------------------------------------------------------
//
// `time.Parse(layout, s)` returns a Time without an explicit Location
// when the layout has no zone segment. Every `time.Parse` call MUST
// either be followed by `.UTC()` OR use a layout that includes a
// timezone token (Z, -07:00, etc.).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_TimeParsedAsUTCAtBoundary(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	parseRE := regexp.MustCompile(`time\.Parse\(\s*("[^"]+")`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		for _, m := range parseRE.FindAllStringSubmatchIndex(body, -1) {
			layout := body[m[2]:m[3]]
			// Layout that includes a TZ token is OK.
			if strings.Contains(layout, "Z") || strings.Contains(layout, "MST") ||
				strings.Contains(layout, "-07") || strings.Contains(layout, "-0700") {
				continue
			}
			// Look for `.UTC()` within 80 chars of the call.
			end := m[1] + 80
			if end > len(body) {
				end = len(body)
			}
			window := body[m[1]:end]
			if strings.Contains(window, ".UTC()") {
				continue
			}
			bad = append(bad, pathToSlash(path)+":"+itoa(lineNumberAt(body, m[0])))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("time.Parse without TZ-bearing layout or .UTC() follow-up:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// R3: TestArch_NoNewTimeFormatStrings
// ----------------------------------------------------------------------------
//
// `time.Format` argument must be a stdlib layout constant
// (RFC3339Nano, RFC3339, Kitchen, etc.) or a constant declared at
// package level — never a string literal. Bare layout strings drift
// across the codebase + break interop.
//
// Soft predicate: bare-string literals in time.Format calls
// outside test code.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoNewTimeFormatStrings(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	formatRE := regexp.MustCompile(`\.Format\(\s*"([^"]+)"\s*\)`)
	var bad []string

	// Layouts that have a stdlib constant equivalent — using the
	// literal is fine (the constant maps to the same string).
	stdlibLayouts := map[string]bool{
		"2006-01-02":                          true, // time.DateOnly
		"15:04:05":                            true, // time.TimeOnly
		"2006-01-02 15:04:05":                 true, // time.DateTime
		"2006-01-02T15:04:05Z07:00":           true, // time.RFC3339
		"2006-01-02T15:04:05.999999999Z07:00": true, // time.RFC3339Nano
	}

	walkGoFiles(t, root, false, func(path string, src []byte) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		body := stripGoComments(string(src))
		for _, m := range formatRE.FindAllStringSubmatch(body, -1) {
			layout := m[1]
			if !regexp.MustCompile(`(01|02|2006|15:04|Mon|Jan)`).MatchString(layout) {
				continue
			}
			if stdlibLayouts[layout] {
				continue
			}
			bad = append(bad, pathToSlash(path)+": "+layout)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("time.Format with non-stdlib inline layout literal — use a constant:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ============================================================================
// Principle MG — additional Go 1.24-1.25 modern-idiom gates (the genuine
// gaps not already covered by ModernForRange / OmitzeroNotOmitempty /
// BenchmarksUseBLoop / TestsUseTContext / ErrorsAsOverTypeAssertion /
// CustomErrorsImplementIs). Each was confirmed already-clean in production
// before gating.
// ============================================================================

// ----------------------------------------------------------------------------
// MG1: TestArch_RangeOverSplitUsesSeq
// ----------------------------------------------------------------------------
//
// Per Go 1.24+: when the ONLY use of a Split/Fields result is to iterate
// it in a `for range`, use the allocation-free iterator variants —
// strings.SplitSeq / strings.FieldsSeq / bytes.SplitSeq / bytes.FieldsSeq —
// instead of strings.Split / strings.Fields / bytes.Split / bytes.Fields
// (which allocate the full []string up front).
//
// Detection: a `*ast.RangeStmt` whose range expression X is a direct call
// to strings.Split / strings.Fields / bytes.Split / bytes.Fields.
//
// Scope: production — non-test files under internal/. The arch-test suite
// + integration-event arch tests legitimately range over strings.Split in
// _test.go (line-by-line source scanning) and are out of scope.
// Production was confirmed clean before this gate landed.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (introduce a production
// `for _, x := range strings.Split(...)` → RED). A committed fixture would
// itself be the banned shape; the AST range-over-Split matcher IS the
// fitness function.
//
// arch-test:no-synctest — purely-static AST analysis test.
func TestArch_RangeOverSplitUsesSeq(t *testing.T) {
	t.Parallel()

	// pkg.Fn → the Seq replacement to suggest.
	splitFns := map[string]map[string]string{
		"strings": {"Split": "SplitSeq", "Fields": "FieldsSeq"},
		"bytes":   {"Split": "SplitSeq", "Fields": "FieldsSeq"},
	}

	type violation struct {
		file string
		line int
		repl string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			rs, ok := n.(*ast.RangeStmt)
			if !ok {
				return true
			}
			call, ok := rs.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			fns, ok := splitFns[pkg]
			if !ok {
				return true
			}
			seq, ok := fns[name]
			if !ok {
				return true
			}
			violations = append(violations, violation{
				file: pathToSlash(path),
				line: fset.Position(rs.Pos()).Line,
				repl: pkg + "." + seq,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d production `for range %s.Split/Fields(...)` loop(s) — use the Go 1.24 iterator variant (no up-front slice allocation):", len(violations), "strings|bytes")
		for _, v := range violations {
			t.Errorf("  %s:%d — range over Split/Fields; use %s instead", v.file, v.line, v.repl)
		}
	}
}

// ----------------------------------------------------------------------------
// MG2: TestArch_WaitGroupUsesWgGo
// ----------------------------------------------------------------------------
//
// Per Go 1.25+: spawning a goroutine tracked by a sync.WaitGroup is
// `wg.Go(fn)` — which captures the Add+Done bookkeeping — NOT the legacy
// `wg.Add(1); go func() { defer wg.Done(); ... }()` triad (one missed
// Done / mis-counted Add is a deadlock or early-return bug).
//
// Detection (low-false-positive, structural): a `go func(){...}()`
// statement whose spawned closure body contains a `wg.Done()` /
// `<x>.Done()` call. The presence of a WaitGroup .Done() inside a
// hand-spawned goroutine is the unambiguous legacy shape — atomic
// counters (.Add(1) on atomic.Int64) never call .Done(), so they don't
// trip. (We key off Done(), not Add(1), precisely to avoid the
// atomic-counter false positive the brief flags.)
//
// Scope: production — non-test files under internal/. The sole production
// WaitGroup user (internal/common/obs/health.go) already uses wg.Go();
// production was confirmed clean before this gate landed.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (rewrite health.go's wg.Go(...) back
// into wg.Add(1)+go func(){defer wg.Done()} → RED). A committed fixture
// would itself be the banned shape; the AST go-func-with-Done matcher IS
// the fitness function.
//
// arch-test:no-synctest — purely-static AST analysis test.
func TestArch_WaitGroupUsesWgGo(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// closureCallsDone reports whether the FuncLit body contains a
	// `<x>.Done()` call (the WaitGroup.Done bookkeeping that wg.Go folds in).
	closureCallsDone := func(lit *ast.FuncLit) bool {
		found := false
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Done" && len(call.Args) == 0 {
				found = true
				return false
			}
			return true
		})
		return found
	}

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			gs, ok := n.(*ast.GoStmt)
			if !ok {
				return true
			}
			lit, ok := gs.Call.Fun.(*ast.FuncLit)
			if !ok || lit.Body == nil {
				return true
			}
			if closureCallsDone(lit) {
				violations = append(violations, violation{
					file: pathToSlash(path),
					line: fset.Position(gs.Pos()).Line,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d hand-spawned `go func(){ defer wg.Done(); ... }()` goroutine(s) — Go 1.25: use wg.Go(fn), which folds the Add/Done bookkeeping in and can't desync the counter:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — go func with wg.Done() (use wg.Go(func(){ ... }))", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// MG3: TestArch_NoHandRolledMinMax
// ----------------------------------------------------------------------------
//
// Per Go 1.21+: `min` and `max` are predeclared builtins for ordered
// types. A hand-rolled top-level `func min(a, b int) int` / `func max(...)`
// shadows the builtin and is dead weight. Ban a top-level (non-method)
// FuncDecl named min/max that takes exactly two same-typed comparable
// (numeric/string) parameters — the builtin's shape. Domain helpers like
// `maxCursorTime(...)` or a 3-arg `min(...)` clamp are NOT named exactly
// min/max with the builtin's 2-arg shape, so they never trip.
//
// Scope: production — non-test files under internal/. The lone hand-rolled
// `func min(a, b int) int` lives in meta_arch_test.go (a _test.go file,
// out of scope); production was confirmed clean before this gate landed.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (add a production `func min(a, b int)
// int` → RED). A committed fixture would itself be the banned shape; the
// AST func-decl matcher IS the fitness function.
//
// arch-test:no-synctest — purely-static AST analysis test.
func TestArch_NoHandRolledMinMax(t *testing.T) {
	t.Parallel()

	numericOrString := map[string]bool{
		"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
		"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
		"uintptr": true, "float32": true, "float64": true, "string": true,
		"byte": true, "rune": true,
	}

	// twoOrderedSameTypedParams reports whether the param list is exactly
	// two parameters of the SAME numeric/string type — the builtin min/max
	// shape. Accepts both `(a, b int)` and `(a int, b int)`.
	twoOrderedSameTypedParams := func(fl *ast.FieldList) bool {
		if fl == nil {
			return false
		}
		var types []string
		for _, fld := range fl.List {
			id, ok := fld.Type.(*ast.Ident)
			if !ok || !numericOrString[id.Name] {
				return false
			}
			n := len(fld.Names)
			if n == 0 {
				n = 1
			}
			for range n {
				types = append(types, id.Name)
			}
		}
		return len(types) == 2 && types[0] == types[1]
	}

	type violation struct {
		file string
		line int
		name string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil { // methods are not the shadow
				continue
			}
			if fd.Name.Name != "min" && fd.Name.Name != "max" {
				continue
			}
			if !twoOrderedSameTypedParams(fd.Type.Params) {
				continue
			}
			violations = append(violations, violation{
				file: pathToSlash(path),
				line: fset.Position(fd.Pos()).Line,
				name: fd.Name.Name,
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d hand-rolled top-level func min/max shadowing the Go 1.21 builtins — delete it and use the predeclared min/max:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — func %s(a, b T) T (use the builtin)", v.file, v.line, v.name)
		}
	}
}

// ----------------------------------------------------------------------------
// MG4: TestArch_NoSortSliceInProduction
// ----------------------------------------------------------------------------
//
// Per Go 1.21+: ordering a slice is `slices.SortFunc(s, cmp)` where the
// comparator returns an int (negative / 0 / positive) — the typed,
// generic idiom. The legacy `sort.Slice(s, less)` / `sort.SliceStable`
// take a `func(i, j int) bool` less-func and sort via runtime reflection
// (reflect.Swapper) — slower, untyped (the closure indexes the slice by
// int, defeating the type system), and superseded. `cmp.Compare` +
// `cmp.Or` express the same orderings (incl. multi-key + DESC via arg
// swap) without reflection.
//
// Detection (AST, low-false-positive): a `*ast.CallExpr` whose callee is
// the qualified selector `sort.Slice` or `sort.SliceStable`. Keyed off
// pkg+name so a method named Slice on some other receiver never trips.
//
// Scope: production — non-test files under internal/. Test code may still
// use sort.Slice freely (this gate's `includeTests=false`). The 9 prior
// production call sites (all per-aggregate FakeRepositories, which the
// project treats as production non-_test.go) were converted to
// slices.SortFunc before this gate landed; production was confirmed clean.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (re-introduce a production
// `sort.Slice(...)` in a fake → RED; revert → GREEN). A committed fixture
// would itself be the banned shape; the AST sort.Slice matcher IS the
// fitness function.
//
// arch-test:no-synctest — purely-static AST analysis test.
func TestArch_NoSortSliceInProduction(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			if pkg != "sort" || (name != "Slice" && name != "SliceStable") {
				return true
			}
			violations = append(violations, violation{
				file: pathToSlash(path),
				line: fset.Position(call.Pos()).Line,
				fn:   "sort." + name,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d production sort.Slice/sort.SliceStable call(s) — Go 1.21: use slices.SortFunc with an int-returning comparator (cmp.Compare / cmp.Or; swap args for DESC) instead of the reflection-based legacy form:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — %s(s, func(i, j int) bool {...}) (use slices.SortFunc(s, func(a, b T) int {...}))", v.file, v.line, v.fn)
		}
	}
}
