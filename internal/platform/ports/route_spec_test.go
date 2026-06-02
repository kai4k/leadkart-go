// TestArch_ guard against drift between the Platform HTTP route table and
// api/openapi.yaml: per ADR 0050 every /api/v1/platform/* route MUST have a
// matching spec operation and vice versa. Mirror of identity's
// route_spec_test.go, scoped to the /api/v1/platform/ prefix.

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

const platformCodeRoutesPath = "http.go"
const platformSpecPath = "../../../api/openapi.yaml"

// platformOwnedPrefixes are the prefixes THIS module owns under the shared
// /api/v1/platform/ namespace; identity owns tenants/persons/impersonation/
// stats (covered by its own spec test). Ownership is by sub-resource segment.
// New sub-resource = append here AND add the spec entry; the gate enforces both.
var platformOwnedPrefixes = []string{
	"/api/v1/platform/unverified-contacts",
	"/api/v1/platform/marketplace/",
	"/api/v1/platform/lead-credits",
}

// matchesOwnedPrefix reports whether p falls under any platformOwnedPrefixes
// namespace ("/foo" covers "/foo" and "/foo/...").
func matchesOwnedPrefix(p string) bool {
	for _, pref := range platformOwnedPrefixes {
		if p == pref || strings.HasPrefix(p, pref+"/") || strings.HasPrefix(p, pref) {
			return true
		}
	}
	return false
}

type platformRouteKey struct {
	method string
	path   string
}

func (r platformRouteKey) String() string { return r.method + " " + r.path }

// TestArch_RouteHasSpecOperation requires code routes and spec operations
// under /api/v1/platform/* to be the same set.
func TestArch_RouteHasSpecOperation(t *testing.T) {
	t.Parallel()

	codeRoutes, err := extractPlatformCodeRoutes(platformCodeRoutesPath)
	if err != nil {
		t.Fatalf("extract code routes: %v", err)
	}
	specRoutes, err := extractPlatformSpecRoutes(platformSpecPath)
	if err != nil {
		t.Fatalf("extract spec routes: %v", err)
	}

	codeSet := make(map[platformRouteKey]struct{}, len(codeRoutes))
	for _, r := range codeRoutes {
		codeSet[r] = struct{}{}
	}
	specSet := make(map[platformRouteKey]struct{}, len(specRoutes))
	for _, r := range specRoutes {
		specSet[r] = struct{}{}
	}

	var codeOrphans, specOrphans []platformRouteKey
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
	slices.SortFunc(codeOrphans, func(a, b platformRouteKey) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(specOrphans, func(a, b platformRouteKey) int { return strings.Compare(a.String(), b.String()) })

	if len(codeOrphans) == 0 && len(specOrphans) == 0 {
		return
	}
	t.Logf("OpenAPI ↔ Platform code-route drift detected (per ADR 0050).")
	t.Logf("Total: %d code routes, %d spec operations under platform-owned prefixes %v",
		len(codeSet), len(specSet), platformOwnedPrefixes)
	for _, r := range codeOrphans {
		t.Errorf("  CODE→ %s — add to api/openapi.yaml or remove the mux.Handle", r)
	}
	for _, r := range specOrphans {
		t.Errorf("  SPEC→ %s — wire in ports.AddRoutes or remove the spec entry", r)
	}
}

func extractPlatformCodeRoutes(path string) ([]platformRouteKey, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []platformRouteKey
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
		if !matchesOwnedPrefix(p) {
			return true
		}
		out = append(out, platformRouteKey{method: method, path: p})
		return true
	})
	return out, nil
}

func extractPlatformSpecRoutes(path string) ([]platformRouteKey, error) {
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
	var out []platformRouteKey
	for specPath, ops := range spec.Paths {
		if !matchesOwnedPrefix(specPath) {
			continue
		}
		for verb := range ops {
			if _, ok := verbs[strings.ToLower(verb)]; !ok {
				continue
			}
			out = append(out, platformRouteKey{
				method: strings.ToUpper(verb),
				path:   specPath,
			})
		}
	}
	return out, nil
}
