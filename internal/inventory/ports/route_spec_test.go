// route_spec_test.go — TestArch_ guard preventing drift between the
// Inventory Go HTTP route table and api/openapi.yaml.
//
// Per ADR 0050: every /api/v1/inventory/* route in code MUST have a
// matching operation in api/openapi.yaml + vice versa.
//
// Mirror of internal/platform/ports/route_spec_test.go, scoped to the
// `/api/v1/inventory/` prefix. Each module owns its own drift gate
// per the canonical layout (PR #31 ADR 0050 §"drift gates are
// PER-MODULE since each module owns its corner of the URL space").

package ports_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const inventoryCodeRoutesPath = "http.go"
const inventorySpecPath = "../../../api/openapi.yaml"

// inventoryOwnedPrefix is the URL namespace THIS module owns. Per
// ADR 0061 (inventory slice 1): single sub-tree `/api/v1/inventory/`.
const inventoryOwnedPrefix = "/api/v1/inventory/"

type inventoryRouteKey struct {
	method string
	path   string
}

func (r inventoryRouteKey) String() string { return r.method + " " + r.path }

// TestArch_RouteHasSpecOperation diffs code-side routes vs spec-side
// operations under /api/v1/inventory/* — both sets must be equal.
//
// Scope: production — the test scans inventory's http.go + the project
// OpenAPI spec; test files are not in scope per ADR 0050.
func TestArch_RouteHasSpecOperation(t *testing.T) {
	t.Parallel()

	codeRoutes, err := extractInventoryCodeRoutes(inventoryCodeRoutesPath)
	if err != nil {
		t.Fatalf("extract code routes: %v", err)
	}
	specRoutes, err := extractInventorySpecRoutes(inventorySpecPath)
	if err != nil {
		t.Fatalf("extract spec routes: %v", err)
	}

	codeSet := make(map[inventoryRouteKey]struct{}, len(codeRoutes))
	for _, r := range codeRoutes {
		codeSet[r] = struct{}{}
	}
	specSet := make(map[inventoryRouteKey]struct{}, len(specRoutes))
	for _, r := range specRoutes {
		specSet[r] = struct{}{}
	}

	var codeOrphans, specOrphans []inventoryRouteKey
	for r := range codeSet {
		if _, ok := specSet[r]; !ok {
			codeOrphans = append(codeOrphans, r)
		}
	}
	for r := range specSet {
		if _, ok := codeSet[r]; !ok {
			specOrphans = append(specOrphans, r)
		}
	}
	slices.SortFunc(codeOrphans, func(a, b inventoryRouteKey) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(specOrphans, func(a, b inventoryRouteKey) int { return strings.Compare(a.String(), b.String()) })

	if len(codeOrphans) == 0 && len(specOrphans) == 0 {
		return
	}
	t.Logf("OpenAPI ↔ Inventory code-route drift detected (per ADR 0050).")
	t.Logf("Total: %d code routes, %d spec operations under %s",
		len(codeSet), len(specSet), inventoryOwnedPrefix)
	for _, r := range codeOrphans {
		t.Errorf("  CODE→ %s — add to api/openapi.yaml or remove the mux.Handle", r)
	}
	for _, r := range specOrphans {
		t.Errorf("  SPEC→ %s — wire in ports.AddRoutes or remove the spec entry", r)
	}
}

func extractInventoryCodeRoutes(path string) ([]inventoryRouteKey, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []inventoryRouteKey
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Handle" || len(call.Args) < 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		raw := strings.Trim(lit.Value, "\"`")
		method, p, found := strings.Cut(raw, " ")
		if !found {
			return true
		}
		if !strings.HasPrefix(p, inventoryOwnedPrefix) {
			return true
		}
		out = append(out, inventoryRouteKey{method: method, path: p})
		return true
	})
	return out, nil
}

func extractInventorySpecRoutes(path string) ([]inventoryRouteKey, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(absPath) //nolint:gosec // arch-test fixture path
	if err != nil {
		return nil, err
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	verbs := map[string]struct{}{
		"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {},
		"head": {}, "options": {}, "trace": {},
	}
	var out []inventoryRouteKey
	for specPath, ops := range spec.Paths {
		if !strings.HasPrefix(specPath, inventoryOwnedPrefix) {
			continue
		}
		for verb := range ops {
			if _, ok := verbs[strings.ToLower(verb)]; !ok {
				continue
			}
			out = append(out, inventoryRouteKey{
				method: strings.ToUpper(verb),
				path:   specPath,
			})
		}
	}
	return out, nil
}
