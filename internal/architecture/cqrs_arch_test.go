// cqrs_arch_test.go — strict CQRS enforcement.
//
// Query handlers must return read models (Views/DTOs), not domain aggregates.
// Returning an aggregate leaks write internals to the port.

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

// TestArch_QueryHandlersReturnReadModelsNotAggregates asserts that every
// `Handle` method in an `app/query` package returns a read model — never a
// pointer to (or a pagination.Page of) a type from a `*/domain/*` package.
//
// Read models (View structs) live in the query package itself, so they
// have no domain-package qualifier and pass. The write aggregate
// (crmlead.CrmLead, product.Product, ...) lives in domain/ and is banned
// from query-handler return signatures.
//
// Scope: production — query handlers in internal/<mod>/app/query/. Test
// fakes that mirror the write Repository interface legitimately reference
// aggregates and are excluded (non _test.go only).
//
// arch-test:no-synctest — purely-static analysis test.
//
// arch-test:no-negative-fixture — the CRM/Inventory query handlers that
// returned the aggregate (pre-fix on this branch) are the recorded
// RED→GREEN proof; a fixture file under app/query would itself be the
// banned shape. The AST return-type matcher IS the fitness function.
func TestArch_QueryHandlersReturnReadModelsNotAggregates(t *testing.T) {
	t.Parallel()

	type violation struct {
		file   string
		line   int
		method string
		typ    string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		queryDir := filepath.Join(internalDir(t), mod, "app", "query")
		walkGoFiles(t, queryDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)

			// alias → import path, so we can tell whether a selector's
			// package is a domain package.
			aliasToPath := map[string]string{}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				alias := ""
				if imp.Name != nil {
					alias = imp.Name.Name
				} else {
					alias = p[strings.LastIndex(p, "/")+1:]
				}
				aliasToPath[alias] = p
			}

			refsDomain := func(expr ast.Expr) (string, bool) {
				return returnTypeRefsDomain(expr, aliasToPath)
			}

			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Name.Name != "Handle" || fd.Type.Results == nil {
					continue
				}
				for _, res := range fd.Type.Results.List {
					if typ, bad := refsDomain(res.Type); bad {
						violations = append(violations, violation{
							file:   pathToSlash(path),
							line:   fset.Position(res.Pos()).Line,
							method: fd.Name.Name,
							typ:    typ,
						})
					}
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("%d query handler(s) return a domain aggregate instead of a read-model View — strict CQRS: project to a *View in the query package; the aggregate must not leak to the port:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — Handle returns %s", v.file, v.line, v.typ)
		}
	}
}

// returnTypeRefsDomain reports whether a return-type expression references a
// */domain/* package, including through *, [], and generic IndexExpr wrappers.
func returnTypeRefsDomain(expr ast.Expr, aliasToPath map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return returnTypeRefsDomain(e.X, aliasToPath)
	case *ast.ArrayType:
		return returnTypeRefsDomain(e.Elt, aliasToPath)
	case *ast.IndexExpr: // Generic[T], e.g. pagination.Page[*product.Product]
		return returnTypeRefsDomain(e.Index, aliasToPath)
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if p, ok := aliasToPath[id.Name]; ok && strings.Contains(p, "/domain/") {
			return id.Name + "." + e.Sel.Name, true
		}
		return "", false
	default:
		return "", false
	}
}
