// test_parity_arch_test.go — Principle TP: Test/Production discipline PARITY.
//
// The Test Discipline catalog (test_discipline_arch_test.go, principle
// TD) ensures tests follow their OWN canon (parallelism, t.Context,
// lifecycle). This file enforces the COMPLEMENTARY rule: every
// discipline rule that applies to production code MUST also apply to
// test code, unless the production-only scope is explicitly
// justified.
//
// The 2026-05-25 SQL-leak finding surfaced the gap: the existing 9
// sqlc arch tests scoped to non-test files via
// `walkGoFiles(..., includeTests=false, ...)`. That blanket
// exemption let 40 raw-SQL sites accumulate in test files over
// 9 waves of integration-test growth. Production was canon-clean;
// tests were the wild west.
//
// User canon mandate (2026-05-25): "Tests are supposed to test the
// code properly. No Dev and Prod test difference. Fix tests properly
// the way they should be done."
//
// Tests in this file (TP1-TP4):
//
//	TP1. TestArch_NoRawSQLInTests
//	TP2. TestArch_TestsWrapErrorsWithPercentW
//	TP3. TestArch_NoSensitiveKeysInTestLogs
//	TP4. TestArch_NoStringInterpInTestSQLConstruction
//	TPMeta. TestMeta_ProductionOnlyRulesAreJustified

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// TP1: TestArch_NoRawSQLInTests
// ----------------------------------------------------------------------------
//
// **The smoking-gun fix.** Test files MUST NOT contain raw SQL string
// literals matching SELECT/INSERT/UPDATE/DELETE/EXPLAIN keywords.
// Per ADR 0004 + user canon mandate: all DB access goes through
// typed helpers (sqlc-generated queries OR test-helper packages
// like `audittest`, `messagingtest`, `rlstest`, `seedtest`).
//
// The proximate trigger: a TD20 mechanical-sweep agent injected
// `// arch-test:ignore-err …` inside a multi-line backtick SQL
// literal because the call site was `_ = pool.QueryRow(t.Context(),
// \`<newline>SELECT ...\`).Scan(&n)`. Typed helpers eliminate the
// multi-line raw-string anti-pattern entirely — bug class becomes
// structurally impossible.
//
// **Allow-list:**
//
//   - `*_explain_integration_test.go` — EXPLAIN gates (ADR 0038)
//     MUST be raw; sqlc doesn't emit EXPLAIN-wrapped queries.
//   - `internal/common/{audit,messaging,pg,...}/*test/*.go` — the
//     typed-helper packages themselves (they wrap the raw SQL by
//     definition).
//   - Files annotated with `// arch-test:raw-sql-justified —
//     <reason>` at the top of the file (last resort).
func TestArch_NoRawSQLInTests(t *testing.T) {
	t.Parallel()

	// Keyword regex: TIGHT shape — only flag UPPERCASE SQL keywords
	// followed by typical SQL syntax (whitespace + identifier, comma,
	// parenthesis, asterisk). Avoids false-positives on Go identifiers
	// like "update@" in emails, "Update" in test names, "INSERT" as
	// a word in test messages, etc.
	//
	// We deliberately do NOT match `EXPLAIN` here because EXPLAIN-gate
	// tests are already file-suffix allow-listed below.
	sqlKwRE := regexp.MustCompile(`\b(SELECT\s+(?:count\(|[a-z_*]|DISTINCT|set_config)|INSERT\s+INTO\b|UPDATE\s+[a-z_]+\s+SET\b|DELETE\s+FROM\b)`)

	type violation struct {
		file string
		line int
		hit  string
	}
	var violations []violation

	walkGoFiles(t, repoRoot(t), true, func(path string, src []byte) {
		slash := pathToSlash(path)
		if !isTestFile(slash) || archTestFile(slash) {
			return
		}
		// Allow-list: EXPLAIN gates (intentionally raw per ADR 0038).
		if strings.HasSuffix(slash, "_explain_integration_test.go") {
			return
		}
		// Allow-list: typed-helper packages themselves.
		if strings.Contains(slash, "/audittest/") ||
			strings.Contains(slash, "/messagingtest/") ||
			strings.Contains(slash, "/rlstest/") ||
			strings.Contains(slash, "/seedtest/") ||
			strings.Contains(slash, "/identitytest/") {
			return
		}
		// Allow-list: the goose migrations test itself — it tests the
		// migrations infrastructure, so raw SQL IS the subject. The
		// migrations_test.go file lives in internal/common/pg/ where
		// the canonical pg helper lives.
		if strings.HasSuffix(slash, "/internal/common/pg/migrations_test.go") {
			return
		}
		text := string(src)
		// Allow-list: file-level annotation.
		if strings.Contains(text, "arch-test:raw-sql-justified") {
			return
		}
		// Strip Go comments first so we don't flag SQL examples in
		// godoc / //-comment blocks.
		stripped := stripGoComments(text)
		lines := strings.Split(stripped, "\n")
		for i, ln := range lines {
			// Skip GRANT statements — testcontainers fixture-time role
			// provisioning. Postgres GRANTs have no sqlc equivalent;
			// this is one-shot boot setup, not query construction.
			if regexp.MustCompile(`\bGRANT\s+`).MatchString(ln) {
				continue
			}
			// Only look at lines containing a quoted string boundary
			// (backtick or double-quote) AND a SQL keyword.
			if !strings.Contains(ln, "`") && !strings.Contains(ln, `"`) {
				continue
			}
			if !sqlKwRE.MatchString(ln) {
				continue
			}
			violations = append(violations, violation{
				file: slash, line: i + 1, hit: strings.TrimSpace(ln),
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d raw-SQL site(s) in test files — use typed helpers (audittest, messagingtest, rlstest, seedtest) per the 2026-05-25 SQL-leak finding. Production canon (ADR 0004) applies to tests too. Allow-list: *_explain_integration_test.go (EXPLAIN gates) + helper packages themselves + file-level `// arch-test:raw-sql-justified — <reason>` annotation:", len(violations))
		for _, v := range violations[:min0(len(violations), 25)] {
			t.Logf("  %s:%d — %s", v.file, v.line, v.hit[:min0(len(v.hit), 80)])
		}
		if len(violations) > 25 {
			t.Logf("  ... %d more", len(violations)-25)
		}
	}
}

// ----------------------------------------------------------------------------
// TP2: TestArch_TestsWrapErrorsWithPercentW
// ----------------------------------------------------------------------------
//
// Sister to TestArch_ErrorWrappingUsesPercentW (production-only).
// Tests use fmt.Errorf to construct test-fixture errors too —
// %v / %s loses errors.Is/As chain just like in production.
//
// Allow-list: t.Errorf / t.Fatalf / t.Logf format strings (those
// are test framework output, not wrapped errors).
func TestArch_TestsWrapErrorsWithPercentW(t *testing.T) {
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
			pkg, name := callPkgAndName(call.Fun)
			if pkg != "fmt" || name != "Errorf" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			fmtLit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			format := fmtLit.Value
			if !strings.Contains(format, "%v") && !strings.Contains(format, "%s") {
				return true
			}
			lastArg := call.Args[len(call.Args)-1]
			lastText := exprText(lastArg)
			if !strings.Contains(strings.ToLower(lastText), "err") {
				return true
			}
			if strings.Contains(format, "%w") {
				return true
			}
			violations = append(violations, violation{
				file: slash, line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d test-file fmt.Errorf call(s) wrap error with %%v/%%s — use %%w to preserve errors.Is/As chain (Russ Cox 'Working with Errors in Go 1.13'). Sister rule to TestArch_ErrorWrappingUsesPercentW (production):", len(violations))
		for _, v := range violations[:min0(len(violations), 25)] {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TP3: TestArch_NoSensitiveKeysInTestLogs
// ----------------------------------------------------------------------------
//
// Sister to TestArch_NoSensitiveFieldsInLogArgs (production-only).
// Tests that pass `password` / `secret` / `api_key` / `private_key`
// as slog field keys risk credential leakage if the test logger
// writes to stdout/CI logs.
//
// Excludes `token` + `jwt` (LeadKart legitimately logs token METADATA
// like token_id, jwt_kid; same exclusion as the production rule).
func TestArch_NoSensitiveKeysInTestLogs(t *testing.T) {
	t.Parallel()

	sensitiveRE := regexp.MustCompile(`(?i)(password|secret|api_key|private_key)`)

	type violation struct {
		file string
		line int
		key  string
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
			pkg, _ := callPkgAndName(call.Fun)
			if pkg != "slog" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok {
					continue
				}
				if !sensitiveRE.MatchString(lit.Value) {
					continue
				}
				violations = append(violations, violation{
					file: slash, line: fset.Position(lit.Pos()).Line, key: lit.Value,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d test-file slog call(s) pass sensitive key (password/secret/api_key/private_key) — same canon as TestArch_NoSensitiveFieldsInLogArgs (production):", len(violations))
		for _, v := range violations[:min0(len(violations), 25)] {
			t.Logf("  %s:%d — %s", v.file, v.line, v.key)
		}
	}
}

// ----------------------------------------------------------------------------
// TP4: TestArch_NoStringInterpInTestSQLConstruction
// ----------------------------------------------------------------------------
//
// Sister to TestArch_NoStringInterpInSQLConstruction (production-only).
// fmt.Sprintf("SELECT ... %s", x) for SQL construction is a SQL
// injection vector + bypasses pgx parameter binding. Same anti-
// pattern in tests.
//
// Allow-list: the test-helper packages (audittest/messagingtest/
// rlstest/seedtest) — they intentionally use fmt.Sprintf with a
// fixed `schema` argument to switch table names (parameter binding
// can't substitute schema/table names in PG).
func TestArch_NoStringInterpInTestSQLConstruction(t *testing.T) {
	t.Parallel()

	sqlKw := regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|WHERE|FROM|JOIN|UNION)\b`)
	sprintfLine := regexp.MustCompile(`(?i)fmt\.Sprintf\(\s*"([^"]*)"`)

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
		// Helper packages allowed — they use fmt.Sprintf with schema
		// substitution for typed wrapping.
		if strings.Contains(slash, "/audittest/") || strings.Contains(slash, "/messagingtest/") ||
			strings.Contains(slash, "/rlstest/") || strings.Contains(slash, "/seedtest/") ||
			strings.Contains(slash, "/identitytest/") {
			return
		}
		// Helper packages' SOURCE files (not tests) — same allow.
		if strings.HasSuffix(slash, "/audittest.go") || strings.HasSuffix(slash, "/outboxtest.go") ||
			strings.HasSuffix(slash, "/inboxtest.go") || strings.HasSuffix(slash, "/rlstest.go") ||
			strings.HasSuffix(slash, "/seedtest.go") || strings.HasSuffix(slash, "/identitytest.go") {
			return
		}
		text := string(src)
		for i, ln := range strings.Split(text, "\n") {
			m := sprintfLine.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			if sqlKw.MatchString(m[1]) {
				violations = append(violations, violation{file: slash, line: i + 1})
			}
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d test-file fmt.Sprintf(\"SELECT/INSERT/UPDATE/DELETE ...\") call(s) — use typed helpers (audittest/messagingtest/rlstest/seedtest) or, in extremis, parameter binding ($1, $2). Sister rule to TestArch_NoStringInterpInSQLConstruction (production):", len(violations))
		for _, v := range violations[:min0(len(violations), 25)] {
			t.Logf("  %s:%d", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// TPMeta: TestMeta_ProductionOnlyRulesAreJustified
// ----------------------------------------------------------------------------
//
// **The institutional fix.** Every arch-test that scopes itself
// production-only (`walkGoFiles(..., includeTests=false, ...)`) MUST
// carry a godoc justification saying WHY tests are excluded. Catches
// future blind-spots at PR time — drift becomes impossible because
// the next reviewer sees the missing rationale.
//
// Acceptable godoc markers (case-insensitive substring match in the
// docblock immediately preceding the function declaration):
//   - `Scope: production` — production-only rule (e.g. domain
//     constructor patterns don't apply to test helpers)
//   - `arch-test:production-only — <reason>` — explicit marker
//   - `tests-covered-by:` — references the TD/TP test that covers
//     the test-side equivalent
//
// This is the META gate that prevents the SQL-leak class of
// findings from recurring on OTHER rules. Every rule must declare
// scope intent — silent test-blindness is forbidden going forward.
func TestMeta_ProductionOnlyRulesAreJustified(t *testing.T) {
	t.Parallel()

	archDir := filepath.Join(internalDir(t), "architecture")

	type violation struct {
		file string
		fn   string
		line int
	}
	var violations []violation

	walkGoFiles(t, archDir, true, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		body := string(src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if !strings.HasPrefix(name, "TestArch_") && !strings.HasPrefix(name, "TestMeta_") {
				continue
			}
			// Scan the function BODY only (not the gap between this
			// func's body close and the next func's docblock) for a
			// walkGoFiles call with the literal `false` argument as
			// the third positional. Use AST.
			callsProdOnly := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "walkGoFiles" {
					return true
				}
				if len(call.Args) < 3 {
					return true
				}
				lit, ok := call.Args[2].(*ast.Ident)
				if !ok || lit.Name != "false" {
					return true
				}
				callsProdOnly = true
				return false
			})
			if !callsProdOnly {
				continue
			}
			// Godoc precedes the func decl. Look at the 1000-char
			// window before the decl start for the scope marker.
			start := int(fd.Pos()) - 1
			docStart := start - 1000
			if docStart < 0 {
				docStart = 0
			}
			doc := strings.ToLower(body[docStart:start])
			if strings.Contains(doc, "scope: production") ||
				strings.Contains(doc, "arch-test:production-only") ||
				strings.Contains(doc, "tests-covered-by:") {
				continue
			}
			violations = append(violations, violation{
				file: pathToSlash(path), fn: name, line: fset.Position(fd.Pos()).Line,
			})
		}
	})

	if len(violations) > 0 {
		t.Errorf("%d arch-test(s) call walkGoFiles(..., includeTests=false, ...) without a scope marker — every production-only rule MUST justify why tests are excluded. Add one of: `// Scope: production — <reason>` OR `arch-test:production-only — <reason>` OR `tests-covered-by: TestArch_<TD>` to the function godoc. Prevents the recurrence of the 2026-05-25 SQL-leak gap (test-side blindness):", len(violations))
		for _, v := range violations[:min0(len(violations), 25)] {
			t.Logf("  %s:%d — %s", v.file, v.line, v.fn)
		}
		if len(violations) > 25 {
			t.Logf("  ... %d more", len(violations)-25)
		}
	}
}
