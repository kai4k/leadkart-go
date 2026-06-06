// route_spec_test.go — TestArch_ guard preventing drift between the Orders
// HTTP route table and api/openapi.yaml per ADR 0050.
package ports_test

import (
	"cmp"
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

const (
	codeRoutesPath = "http.go"
	specPath       = "../../../api/openapi.yaml"
	ordersPrefix   = "/api/v1/orders/"
)

type routeKey struct {
	method string
	path   string
}

func (r routeKey) String() string { return r.method + " " + r.path }

func TestArch_RouteHasSpecOperation(t *testing.T) {
	t.Parallel()

	codeRoutes, err := extractCodeRoutes(codeRoutesPath)
	if err != nil {
		t.Fatalf("extract code routes: %v", err)
	}
	specRoutes, err := extractSpecRoutes(specPath)
	if err != nil {
		t.Fatalf("extract spec routes: %v", err)
	}

	codeSet := make(map[routeKey]struct{}, len(codeRoutes))
	for _, r := range codeRoutes {
		codeSet[r] = struct{}{}
	}
	specSet := make(map[routeKey]struct{}, len(specRoutes))
	for _, r := range specRoutes {
		specSet[r] = struct{}{}
	}

	var codeOrphans, specOrphans []routeKey
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
	slices.SortFunc(codeOrphans, func(a, b routeKey) int { return cmp.Compare(a.String(), b.String()) })
	slices.SortFunc(specOrphans, func(a, b routeKey) int { return cmp.Compare(a.String(), b.String()) })

	if len(codeOrphans) == 0 && len(specOrphans) == 0 {
		return
	}
	t.Logf("OpenAPI ↔ code-route drift detected (per ADR 0050).")
	t.Logf("Total: %d code routes, %d spec operations under %s*", len(codeSet), len(specSet), ordersPrefix)
	if len(codeOrphans) > 0 {
		t.Logf("\n%d route(s) in code but missing from api/openapi.yaml:", len(codeOrphans))
		for _, r := range codeOrphans {
			t.Errorf("  CODE→ %s — add an operation to api/openapi.yaml or remove the mux.Handle", r)
		}
	}
	if len(specOrphans) > 0 {
		t.Logf("\n%d operation(s) in api/openapi.yaml but not registered in code:", len(specOrphans))
		for _, r := range specOrphans {
			t.Errorf("  SPEC→ %s — register the route in ports.AddRoutes or remove the spec entry", r)
		}
	}
}

func extractCodeRoutes(path string) ([]routeKey, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []routeKey
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
		if !strings.HasPrefix(p, ordersPrefix) {
			return true
		}
		out = append(out, routeKey{method: method, path: p})
		return true
	})
	return out, nil
}

func extractSpecRoutes(path string) ([]routeKey, error) {
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
	var out []routeKey
	for specPath, ops := range spec.Paths {
		if !strings.HasPrefix(specPath, ordersPrefix) {
			continue
		}
		for verb := range ops {
			if _, ok := verbs[strings.ToLower(verb)]; !ok {
				continue
			}
			out = append(out, routeKey{method: strings.ToUpper(verb), path: specPath})
		}
	}
	return out, nil
}
