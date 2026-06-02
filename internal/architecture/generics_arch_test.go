// generics_arch_test.go — Principle K: Generics discipline.
//
// Generics are used for containers (pagination.Page[T]) and to eliminate
// untyped `any` returns. Over-abstraction is an anti-pattern (ILT talks).

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_PaginationUsesGenericPage asserts query handlers returning *Page
// use pagination.Page[T], not hand-rolled types (ADR 0038).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PaginationUsesGenericPage(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(root, mod, "app", "query")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			body := stripGoComments(string(src))
			// Find function returns that look like `*FooPage` /
			// `FooPage` (not `pagination.Page`).
			re := regexp.MustCompile(`\)\s*\(\s*\*?(\w+Page)\b,\s*error\s*\)`)
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				name := m[1]
				if name == "Page" {
					// Bare `Page` is the generic alias usage. OK.
					continue
				}
				// Ensure pagination.Page is referenced somewhere in the file.
				if !strings.Contains(body, "pagination.Page[") {
					bad = append(bad, pathToSlash(path)+": returns "+name)
				}
			}
		})
	}

	if len(bad) > 0 {
		t.Fatalf("query handler returns hand-rolled *Page type — use pagination.Page[T] (ADR 0038):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_NoAnyInExportedReturns asserts exported functions in app/ and
// adapters/ don't return any or interface{}.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoAnyInExportedReturns(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	for _, mod := range modulesUnderInternal(t) {
		for _, layer := range []string{"app", "adapters"} {
			dir := filepath.Join(root, mod, layer)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				slash := pathToSlash(path)
				if strings.Contains(slash, "/adapters/db/") {
					return
				}
				_, file := parseFile(t, path, src)
				ast.Inspect(file, func(n ast.Node) bool {
					fd, ok := n.(*ast.FuncDecl)
					if !ok || !fd.Name.IsExported() {
						return true
					}
					if fd.Type.Results == nil {
						return true
					}
					for _, r := range fd.Type.Results.List {
						if id, ok := r.Type.(*ast.Ident); ok && id.Name == "any" {
							bad = append(bad, slash+": "+fd.Name.Name+" returns any")
						}
						if it, ok := r.Type.(*ast.InterfaceType); ok && it.Methods != nil && len(it.Methods.List) == 0 {
							bad = append(bad, slash+": "+fd.Name.Name+" returns interface{}")
						}
					}
					return true
				})
			})
		}
	}

	if len(bad) > 0 {
		t.Fatalf("exported fn returns any/interface{} — use typed result or generics:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_GenericConstraintsExplicit asserts exported generic types/funcs
// don't use bare `[T any]` without arch-test:bare-any-generic opt-out.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_GenericConstraintsExplicit(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	bareAnyRE := regexp.MustCompile(`\[(\w+)\s+any\]`)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		body := string(src)
		lines := strings.Split(body, "\n")
		for i, ln := range lines {
			if !bareAnyRE.MatchString(ln) {
				continue
			}
			if strings.Contains(ln, "arch-test:bare-any-generic") {
				continue
			}
			// Unexported funcs/types: package-private, skip.
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "func ") {
				// Walk back to the func keyword on the same line; check
				// the name capitalisation.
				if idx := strings.Index(trimmed, "func "); idx >= 0 {
					rest := trimmed[idx+5:]
					// Skip receiver parens.
					if strings.HasPrefix(rest, "(") {
						if close := strings.Index(rest, ")"); close >= 0 {
							rest = strings.TrimSpace(rest[close+1:])
						}
					}
					if len(rest) > 0 && rest[0] >= 'a' && rest[0] <= 'z' {
						continue
					}
				}
			}
			if strings.HasPrefix(trimmed, "type ") {
				rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "type "))
				if len(rest) > 0 && rest[0] >= 'a' && rest[0] <= 'z' {
					continue
				}
			}
			bad = append(bad, slash+":"+itoa(i+1))
		}
	})

	if len(bad) > 0 {
		t.Fatalf("exported generic uses `[T any]` without explicit constraint (forward-compat hazard):\n  %s",
			strings.Join(bad, "\n  "))
	}
}
