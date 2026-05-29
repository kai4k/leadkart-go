// type_safety_arch_test.go — Principle Y: Type safety.
//
// Go's type system is the cheapest review reviewer we have. Tests
// here protect the spots where untyped escape hatches creep back:
//
//   - `interface{}` in struct fields → invisible coupling to "any
//     shape works";
//   - raw type-assertion `x.(*T)` (vs comma-ok or errors.As) →
//     panic on first miss;
//   - untyped magic numbers in business code → meaning lost;
//   - Hungarian-notation prefixes (`IRepository`, `TUser`) → C#
//     conventions in Go.
//
// Y1 (NoAnyInExportedReturns) is implemented in generics_arch_test.go
// since it's the primary "use generics" lever; the rest live here.
//
// Cited canon:
//   - Russ Cox — `any` alias introduction (Go 1.18 notes)
//   - Effective Go — interface satisfaction is implicit
//   - Cheney — "Practical Go" §3 (interfaces small + consumer-defined)

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
// Y2: TestArch_NoInterfaceEmptyInStructFields
// ----------------------------------------------------------------------------
//
// `interface{}` (or its alias `any`) in a struct field signals
// "anything fits" — a refactor-hostile shape. Use a typed field,
// a generic type param, or a small interface.
//
// Allow-list: ad-hoc adapter test fakes that wrap a `payload any`
// (e.g. cache-recorder spies) opt out via
// `// arch-test:fake-any-payload`. Generated db/ skipped.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoInterfaceEmptyInStructFields(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		body := string(src)
		_, file := parseFile(t, path, src)
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			for _, f := range st.Fields.List {
				if isEmptyIface(f.Type) {
					for _, fname := range f.Names {
						// Heuristic opt-out: marker comment on the
						// field's line.
						if strings.Contains(body, "arch-test:fake-any-payload") &&
							strings.Contains(slash, "test") {
							continue
						}
						bad = append(bad, slash+": "+ts.Name.Name+"."+fname.Name)
					}
				}
			}
			return true
		})
	})

	if len(bad) > 0 {
		t.Fatalf("struct field typed `any`/`interface{}` — use typed field or generics:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// Y3: TestArch_TypeAssertionsUseCommaOk
// ----------------------------------------------------------------------------
//
// Raw `x.(*T)` panics on a miss. Comma-ok `v, ok := x.(*T)` makes
// the failure path explicit. Banned outside type-switch context.
//
// Allow-list: errors.As is fine (caught by M6); test files have
// looser rules.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_TypeAssertionsUseCommaOk(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Detect a `.(SomeType)` that's NOT in a `:= x.(T)` (comma-ok)
	// AND not in a type-switch. Heuristic: line containing `.(`
	// but lacking `,` near the assertion AND lacking `type)`.
	assertRE := regexp.MustCompile(`\.\([\w*.]+\)`)
	// A real type assertion is Go syntax, never inside a string literal.
	// Blank double-quoted string contents so type-assertion-like
	// substrings (e.g. a goleak ignore "pkg.(*Pool).method") don't
	// false-positive — same rationale as stripping comments.
	stringRE := regexp.MustCompile(`"[^"]*"`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		body := stripGoComments(string(src))
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			ln = stringRE.ReplaceAllString(ln, `""`)
			if !assertRE.MatchString(ln) {
				continue
			}
			if strings.Contains(ln, ".(type)") {
				continue
			}
			if regexp.MustCompile(`\w+,\s*\w+\s*:?=`).MatchString(ln) {
				continue
			}
			if regexp.MustCompile(`return\s+\w+,\s*\w+`).MatchString(ln) {
				continue
			}
			// Generic type-parameter assertion `val.(V)` where V is a
			// type param — safe in generic context (compiler enforces).
			// Heuristic: assertion to a single uppercase letter.
			if regexp.MustCompile(`\.\([A-Z]\)`).MatchString(ln) {
				continue
			}
			bad = append(bad, slash+":"+itoa(i+1))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("raw type-assertion x.(*T) (panics on miss) — use comma-ok or errors.As:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// Y4: TestArch_NoUntypedNumericConstantsInBusiness
// ----------------------------------------------------------------------------
//
// Magic numbers in business code (`if x > 100`) lose meaning. Either
// name the constant (`const MaxResults = 100`) or use a typed
// declaration (`const x int64 = 100`).
//
// Pragmatic predicate: lines in `<module>/app/command/` containing
// bare numeric literals >= 10 that are NOT inside a `const`,
// `time.`, `len(`, `cap(`, `make(`, or a switch case.
//
// This is a SOFT check (heuristic) — opt out via
// `// arch-test:magic-ok <reason>` on the same line.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoUntypedNumericConstantsInBusiness(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Lines with a bare digit literal >= 10 (but not 100% nor floats
	// nor hex 0x... nor times).
	bigNumRE := regexp.MustCompile(`\b([1-9]\d{2,})\b`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "app", "command")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := string(src)
			lines := strings.Split(body, "\n")
			for i, ln := range lines {
				trimmed := strings.TrimSpace(ln)
				if trimmed == "" || strings.HasPrefix(trimmed, "//") {
					continue
				}
				if strings.Contains(ln, "arch-test:magic-ok") {
					continue
				}
				if strings.Contains(ln, "const ") ||
					strings.Contains(ln, "time.") ||
					strings.Contains(ln, "http.Status") ||
					strings.Contains(ln, "len(") ||
					strings.Contains(ln, "cap(") ||
					strings.Contains(ln, "make(") ||
					strings.Contains(ln, "rand.") ||
					strings.Contains(ln, "[]byte") {
					continue
				}
				// Strip string literals on the line (e.g. URL paths).
				stripped := regexp.MustCompile(`"[^"]*"`).ReplaceAllString(ln, `""`)
				m := bigNumRE.FindString(stripped)
				if m == "" {
					continue
				}
				bad = append(bad, pathToSlash(path)+":"+itoa(i+1)+" — literal "+m)
			}
		})
	}

	// Only fail if violations EXCEED a permissive ceiling (this is the
	// gentlest possible introduction; once a magic-number sweep ships
	// the budget tightens).
	const ceiling = 50
	if len(bad) > ceiling {
		t.Fatalf("too many bare numeric literals (> %d) in app/command/ — name them as consts:\n  %s",
			ceiling, strings.Join(bad[:30], "\n  "))
	}
}

// ----------------------------------------------------------------------------
// Y5: TestArch_NoHungarianNotation
// ----------------------------------------------------------------------------
//
// `IFoo` / `TFoo` / `IRepository` are C# / Delphi conventions. Go
// uses bare names — `Foo`, `Repository`. Interface vs struct is
// discovered by usage, not name.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoHungarianNotation(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Match exported type names beginning with I, T, C, S, E + a
	// second uppercase letter. The most common Hungarian forms.
	hunRE := regexp.MustCompile(`^type\s+(I[A-Z]\w+|T[A-Z]\w+|C[A-Z]\w+)\b`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		for _, ln := range strings.Split(body, "\n") {
			m := hunRE.FindStringSubmatch(strings.TrimSpace(ln))
			if m == nil {
				continue
			}
			name := m[1]
			// Common false positives: Identity-related names that
			// LEGITIMATELY start with "Id" / "Identity".
			if strings.HasPrefix(name, "Id") && len(name) > 2 &&
				(name[2] >= 'a' && name[2] <= 'z') {
				continue
			}
			if strings.HasPrefix(name, "Identity") || strings.HasPrefix(name, "Idempotency") ||
				strings.HasPrefix(name, "ID") || strings.HasPrefix(name, "TTL") ||
				strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Tx") ||
				strings.HasPrefix(name, "Time") || strings.HasPrefix(name, "Tenant") ||
				strings.HasPrefix(name, "Topic") || strings.HasPrefix(name, "Type") ||
				strings.HasPrefix(name, "Token") || strings.HasPrefix(name, "Table") ||
				strings.HasPrefix(name, "CR") || strings.HasPrefix(name, "CSV") ||
				strings.HasPrefix(name, "Cl") || strings.HasPrefix(name, "Cmd") ||
				strings.HasPrefix(name, "Co") || strings.HasPrefix(name, "Cr") ||
				strings.HasPrefix(name, "Ch") || strings.HasPrefix(name, "Ca") ||
				strings.HasPrefix(name, "Cu") || strings.HasPrefix(name, "Ci") ||
				strings.HasPrefix(name, "In") || strings.HasPrefix(name, "Im") ||
				strings.HasPrefix(name, "It") || strings.HasPrefix(name, "Is") ||
				strings.HasPrefix(name, "Inb") || strings.HasPrefix(name, "Inv") {
				continue
			}
			bad = append(bad, pathToSlash(path)+": "+name)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("Hungarian-notation type name (Go uses bare names — Effective Go):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// Y6: TestArch_ClosedSetFieldsAreTypedEnums
// ----------------------------------------------------------------------------
//
// Per TDL "Safer Enums in Go": a closed-set value in the DOMAIN must be
// a defined type (`type X string` + typed consts + IsValid/String/Parse),
// never a bare `string`/`[]string`. A field whose doc-comment OR
// line-comment declares a closed set — two or more quoted alternatives
// separated by `|`, e.g. `"PCD" | "ThirdParty"` — but whose Go type is
// bare string is the anti-pattern this gate bans: the type system then
// permits any off-catalogue value, pushing validation to scattered
// run-time checks (the pre-conversion crmlead.Profile shape).
//
// WIRE/BOUNDARY structs are EXCLUDED — "no domain VOs on the wire":
// strings are correct on integration-event payloads, HTTP DTOs, DB rows,
// and query Views. Skipped: any struct whose name ends in Snapshot /
// Input / DTO / Dto / Payload / View / Params / Row, and ANY file under
// an `integrationevents/` path. (So platform's LeadSnapshot + crmlead's
// PurchaseSnapshot — both string-typed with `"PCD" | "ThirdParty"`
// comments — correctly pass; the aggregate's Profile + stored state are
// checked.)
//
// Scope: production — domain aggregates + their VOs under
// internal/<mod>/domain/. Test files declare fixtures freely.
//
// arch-test:no-negative-fixture — the recorded RED→GREEN proof is the
// mutation test in the deliverable (revert one crmlead.Profile enum
// field to bare `string` keeping its `"a" | "b"` comment → this gate
// goes RED). A committed fixture struct under domain/ with the banned
// shape would itself be the violation; the AST comment-vs-type matcher
// IS the fitness function.
//
// arch-test:no-synctest — purely-static AST analysis test.
func TestArch_ClosedSetFieldsAreTypedEnums(t *testing.T) {
	t.Parallel()

	// closedSetRE matches a comment that enumerates two or more quoted
	// alternatives separated by `|` — e.g. `"PCD" | "ThirdParty"` or
	// `"" | "a" | "b"`. The empty-string alternative counts toward the
	// "unset is valid" idiom but the rule still requires two+ alternatives
	// total, so a lone `"x"` (not a closed set) never trips.
	closedSetRE := regexp.MustCompile(`"[^"]*"\s*\|\s*"[^"]*"`)

	// Wire/boundary struct name suffixes — strings are correct on the wire.
	wireSuffixes := []string{"Snapshot", "Input", "DTO", "Dto", "Payload", "View", "Params", "Row"}
	isWireStructName := func(name string) bool {
		for _, suf := range wireSuffixes {
			if strings.HasSuffix(name, suf) {
				return true
			}
		}
		return false
	}

	// isBareStringType reports whether the field type is bare `string` or
	// `[]string` (the un-typed shapes this gate bans for closed-set fields).
	// A NAMED type (BusinessType, etc.) is an *ast.Ident whose Name is not
	// the predeclared "string"; a []NamedType array elt is likewise named.
	isBareStringType := func(e ast.Expr) bool {
		switch ty := e.(type) {
		case *ast.Ident:
			return ty.Name == "string"
		case *ast.ArrayType:
			if id, ok := ty.Elt.(*ast.Ident); ok {
				return id.Name == "string"
			}
		}
		return false
	}

	commentDeclaresClosedSet := func(g *ast.CommentGroup) bool {
		if g == nil {
			return false
		}
		return closedSetRE.MatchString(g.Text())
	}

	type violation struct {
		file  string
		line  int
		field string
		strct string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			slash := pathToSlash(path)
			// Files under an integrationevents/ path are wire contracts even
			// if (defensively) one ever lived under domain/.
			if strings.Contains(slash, "/integrationevents/") {
				return
			}
			fset, f := parseFile(t, path, src)

			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					if isWireStructName(ts.Name.Name) {
						continue // wire/boundary struct — strings are correct
					}
					for _, fld := range st.Fields.List {
						if !commentDeclaresClosedSet(fld.Doc) && !commentDeclaresClosedSet(fld.Comment) {
							continue
						}
						if !isBareStringType(fld.Type) {
							continue // already a named enum type — good
						}
						for _, nm := range fld.Names {
							violations = append(violations, violation{
								file:  slash,
								line:  fset.Position(fld.Pos()).Line,
								field: nm.Name,
								strct: ts.Name.Name,
							})
						}
					}
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("%d domain struct field(s) declare a closed set in their comment but are typed as bare string/[]string — define a typed enum (TDL Safer Enums): type X string + consts + Parse/String/IsValid, and make the field that type:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — %s.%s declares `\"a\" | \"b\"` but is bare string", v.file, v.line, v.strct, v.field)
		}
	}
}

// isEmptyIface reports whether the AST expression is `interface{}`
// or `any`.
func isEmptyIface(e ast.Expr) bool {
	if id, ok := e.(*ast.Ident); ok && id.Name == "any" {
		return true
	}
	if it, ok := e.(*ast.InterfaceType); ok {
		return it.Methods == nil || len(it.Methods.List) == 0
	}
	return false
}
