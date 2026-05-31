// error_handling_arch_test.go — Principle M: Error handling discipline.
//
// Cheney + Go 1.13 errors package: sentinel errors, no magic-string matching,
// no panic(string), wrap with %w, custom error types implement Is(),
// domain errors use debug-tier text (HTTP layer maps to wire).

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_SentinelErrorsNamedErr asserts package-level errors.New() is
// assigned to a var ErrXxx.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_SentinelErrorsNamedErr(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Look for `errors.New("...")` that's NOT preceded on the same
	// line by `var ErrXxx =`.
	pkgLvlNewRE := regexp.MustCompile(`(?m)^(?:var\s+)?(\w+)\s*=\s*errors\.New\(`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		for _, m := range pkgLvlNewRE.FindAllStringSubmatch(body, -1) {
			name := m[1]
			if !strings.HasPrefix(name, "Err") && !strings.HasPrefix(name, "err") {
				bad = append(bad, pathToSlash(path)+": "+name+" = errors.New(...)")
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("package-level errors.New() not named Err*/err* (Go errors-package convention):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_NoErrorDotMessageInProduction asserts err.Error() is absent from
// app/ and adapters/ outside log/HTTP layers.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoErrorDotMessageInProduction(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Match `err.Error()` (or `e.Error()`, `xerr.Error()` etc.).
	errMethodRE := regexp.MustCompile(`\b\w*[Ee]rr\w*\.Error\(\s*\)`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"app", "adapters"} {
			dir := filepath.Join(root, mod, layer)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				slash := pathToSlash(path)
				if strings.Contains(slash, "/adapters/db/") {
					return
				}
				body := string(src)
				// Strip Error() method bodies so they don't self-flag.
				errMethodBodyRE := regexp.MustCompile(`(?s)func\s*\([^)]+\)\s+Error\(\)\s+string\s*\{[^}]*\}`)
				body = errMethodBodyRE.ReplaceAllString(body, "func errStripped() string { return \"\" }")
				lines := strings.Split(body, "\n")
				for i, ln := range lines {
					if !errMethodRE.MatchString(ln) {
						continue
					}
					if strings.Contains(ln, "slog.") || strings.Contains(ln, "log.") {
						continue
					}
					if strings.Contains(ln, "writeError") ||
						strings.Contains(ln, "writeProblem") ||
						strings.Contains(ln, "ErrorResponse{") {
						continue
					}
					bad = append(bad, slash+":"+itoa(i+1))
				}
			})
		}
	}

	if len(bad) > 0 {
		t.Fatalf("err.Error() outside log/HTTP layer — use errors.Is/As (text matching is fragile):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_CustomErrorsImplementIs asserts error types wrapping a sentinel
// field also implement Is(). Opt out via arch-test:no-errors-is.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_CustomErrorsImplementIs(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		modDir := filepath.Join(root, mod)
		walkGoFiles(t, modDir, false, func(path string, src []byte) {
			_, file := parseFile(t, path, src)
			info := map[string]struct {
				hasError, hasIs, hasSentinel, optOut bool
			}{}
			for _, d := range file.Decls {
				gd, ok := d.(*ast.GenDecl)
				if ok {
					for _, sp := range gd.Specs {
						ts, ok := sp.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						rec := info[ts.Name.Name]
						for _, f := range st.Fields.List {
							if id, ok := f.Type.(*ast.Ident); ok && id.Name == "error" {
								for _, n := range f.Names {
									if strings.Contains(strings.ToLower(n.Name), "sentinel") ||
										strings.Contains(strings.ToLower(n.Name), "wrapped") ||
										n.Name == "Inner" {
										rec.hasSentinel = true
									}
								}
							}
						}
						info[ts.Name.Name] = rec
					}
				}
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
					continue
				}
				recvType := exprString(fd.Recv.List[0].Type)
				recvType = strings.TrimPrefix(recvType, "*")
				rec := info[recvType]
				if fd.Name.Name == "Error" {
					rec.hasError = true
				}
				if fd.Name.Name == "Is" {
					rec.hasIs = true
				}
				info[recvType] = rec
			}
			// Check opt-out comments per-type via raw source scan.
			body := string(src)
			for name, rec := range info {
				if !rec.hasError || !rec.hasSentinel || rec.hasIs {
					continue
				}
				if strings.Contains(body, "type "+name+" ") &&
					strings.Contains(body, "arch-test:no-errors-is") {
					continue
				}
				bad = append(bad, pathToSlash(path)+": "+name)
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("error type wraps a sentinel field but lacks Is() — errors.Is won't traverse:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_NoPanicString asserts panic("...") is absent. Use panic(fmt.Errorf(...))
// or return an error.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoPanicString(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if !strings.HasPrefix(trimmed, "panic(\"") {
				continue
			}
			// Allow the canonical guard pattern: panic after a nil/zero check.
			isGuard := false
			for j := i - 1; j >= 0 && j > i-4; j-- {
				p := strings.TrimSpace(lines[j])
				if strings.HasPrefix(p, "if ") &&
					(strings.Contains(p, "== nil") || strings.Contains(p, `== ""`) ||
						strings.Contains(p, "<") || strings.Contains(p, "len(") ||
						strings.Contains(p, "!") || strings.Contains(p, ".Valid()") ||
						strings.Contains(p, ".IsZero()") || strings.Contains(p, ".Empty()")) {
					isGuard = true
					break
				}
			}
			if isGuard {
				continue
			}
			bad = append(bad, pathToSlash(path)+":"+itoa(i+1))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("panic(\"...\") loses structured chain — use panic(fmt.Errorf(...)) or return:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_NoMessageStringMatching asserts strings.Contains(err.Error(), ...)
// is absent. Use errors.Is + typed sentinel.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoMessageStringMatching(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	matchRE := regexp.MustCompile(`strings\.Contains\(\s*\w*[Ee]rr\w*\.Error\(\)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		if matchRE.MatchString(body) {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("strings.Contains(err.Error(), ...) — use errors.Is + typed sentinel:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_ErrorsAsOverTypeAssertion asserts raw err.(*T) type-assertions are
// absent. Use errors.As (wrap-safe).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_ErrorsAsOverTypeAssertion(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Raw assert on an err-named symbol: `err.(*FooErr)` outside
	// type-switch context.
	assertRE := regexp.MustCompile(`\b\w*[Ee]rr\w*\.\(\*?\w+(?:\.\w+)?\)`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		// Strip type-switch blocks (`err.(type)`).
		body = regexp.MustCompile(`\b\w*[Ee]rr\w*\.\(type\)`).ReplaceAllString(body, "")
		if assertRE.MatchString(body) {
			bad = append(bad, pathToSlash(path))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("raw err.(*T) assertion — use errors.As (Go 1.13+ wrap-safe):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_PackageExportsErrSentinels asserts every domain aggregate package
// exports at least one ErrXxx sentinel.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PackageExportsErrSentinels(t *testing.T) {
	t.Parallel()

	pureVO := map[string]bool{
		"errs": true, "ids": true, "slug": true, "email": true,
		"phone": true, "pan": true, "gst": true, "postaladdress": true,
		"druglicence": true, "pagination": true, "tenancy": true,
	}

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(root, mod, "domain")
		entries, err := readDirSafe(domainDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if pureVO[e.Name()] {
				continue
			}
			pkgDir := filepath.Join(domainDir, e.Name())
			hasErr := false
			walkGoFiles(t, pkgDir, false, func(_ string, src []byte) {
				body := stripGoComments(string(src))
				if regexp.MustCompile(`(?m)^(?:var\s+)?Err\w+\s*=`).MatchString(body) {
					hasErr = true
				}
			})
			if !hasErr {
				bad = append(bad, mod+"/domain/"+e.Name())
			}
		}
	}

	if len(bad) > 0 {
		t.Fatalf("domain aggregate package exports no Err* sentinel (handlers can't map errors):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_DomainErrorsNoUserStrings asserts domain Err sentinel messages don't
// look user-facing (sentence-case with period, or "Please ...").
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_DomainErrorsNoUserStrings(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	// Match `Err... = errors.New("...")`.
	errLitRE := regexp.MustCompile(`Err\w+\s*=\s*errors\.New\("([^"]+)"`)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(root, mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			for _, m := range errLitRE.FindAllStringSubmatch(body, -1) {
				msg := m[1]
				// User-string heuristic: starts with capital + has
				// spaces + ends with period OR contains "Please".
				if strings.Contains(msg, "Please ") {
					bad = append(bad, pathToSlash(path)+": "+truncate(msg, 50))
					continue
				}
				if len(msg) > 0 && msg[0] >= 'A' && msg[0] <= 'Z' &&
					strings.Contains(msg, " ") &&
					strings.HasSuffix(msg, ".") {
					bad = append(bad, pathToSlash(path)+": "+truncate(msg, 50))
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("domain sentinel message looks user-facing — HTTP layer maps; domain text is debug-tier:\n  %s",
			strings.Join(bad, "\n  "))
	}
}
