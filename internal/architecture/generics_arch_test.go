// generics_arch_test.go — Principle K: Generics discipline.
//
// Go generics (1.18+) are a precision tool: they let us shed
// `interface{}` from container types + cursor types + reusable
// service shapes. The risk is the cousin pattern of "generics
// everywhere" — over-abstraction is a known anti-pattern (Ian Lance
// Taylor's own talks warn about this).
//
// These tests guard the spots where generics are the canon: the
// pagination.Page type + reusable container ctors. They also ban
// fresh `any`/`interface{}` returns from public APIs (those signal
// "I didn't decide on the type yet" — anti-pattern in adapters).
//
// Cited canon:
//   - Russ Cox / Ian Lance Taylor — Go 1.18 generics blog series
//   - Adam Lee — "Don't use generics, use generics for collections"
//   - LeadKart pagination.Page[T] design

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// K1: TestArch_PaginationUsesGenericPage
// ----------------------------------------------------------------------------
//
// `pagination.Page[T]` is the canonical return shape for
// keyset-paginated queries. Hand-rolled `XxxListPage` / `XxxResult`
// types in adapters are drift — they bypass the standard
// has_more/next_cursor envelope.
//
// Predicate: any function in `<module>/app/query/` returning a
// type whose name ends in `Page` MUST use `pagination.Page[T]`.
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

// ----------------------------------------------------------------------------
// K2: TestArch_NoAnyInExportedReturns
// ----------------------------------------------------------------------------
//
// Exported functions in `app/` + `adapters/` (non-generated) may
// not return `any` or `interface{}` as a final result type. Untyped
// returns force the caller to type-assert + that's a refactor-hostile
// API surface. Use a typed result, an error-only return, or a
// well-named generic constraint.
//
// Excludes: test files (free-form), generated db/, json-marshal-shape
// helpers in common/ openly typed `any` for the json package.
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

// ----------------------------------------------------------------------------
// K3: TestArch_GenericConstraintsExplicit
// ----------------------------------------------------------------------------
//
// Generic type parameters use a constraint — `comparable`, `any`,
// `cmp.Ordered`, or a domain-named constraint. We reject `[T any]`
// at exported package boundaries: the unconstrained generic is a
// future-incompatibility hazard + the constraint should be the
// minimum the body actually requires.
//
// Allow-list: containers that legitimately want `any` (e.g. generic
// Page container) opt out via `// arch-test:bare-any-generic
// <reason>` on the same line as the type-param decl.
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
			// Allow internal helper fns (lowercase first letter of the
			// surrounding decl): these are package-private and the
			// constraint is the consumer's problem.
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
