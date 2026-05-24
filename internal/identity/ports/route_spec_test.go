// route_spec_test.go — TestArch_ guard preventing drift between the
// Go HTTP route table and api/openapi.yaml.
//
// Per ADR 0050: the OpenAPI spec is the canonical contract (ADR 0046);
// the Go routes MUST match it exactly under /api/v1/*. This arch test
// is the gate.
//
// Two failure modes caught:
//
//   - code orphans: a `mux.Handle("METHOD /api/v1/...", h)` registration
//     with no matching operation in api/openapi.yaml. Either add the
//     spec entry OR remove the route.
//   - spec orphans: an operation in api/openapi.yaml with no matching
//     mux.Handle in code. Either ship the handler OR remove the spec
//     entry.
//
// Scope: only /api/v1/* routes are checked. Infrastructure routes
// (`GET /`, `GET /favicon.ico`, `GET /docs`, `GET /openapi.yaml`)
// live OUTSIDE the spec on purpose — they're cross-cutting, not
// product API surface.
//
// History: pre-Wave-9, the spec was authored once in Wave 5 and
// promptly diverged from later route additions (Wave 4 impersonation
// added `POST /v1/platform/impersonation/sessions`; Wave 3 added
// `GET /v1/platform/persons?email=`). No CI gate. This test is the
// gate.

package ports_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// codeRoutesPaths are the source files whose mux.Handle calls are the
// code-side truth. Every module that registers /api/v1/* routes via
// its `ports.AddRoutes` MUST have its http.go listed here, otherwise
// the drift gate silently misses cross-module routes.
//
// Per ADR 0050 (Wave 9.3) + ADR 0061 (inventory slice 1):
var codeRoutesPaths = []string{
	"http.go",
	"../../inventory/ports/http.go",
}

// specPath is the canonical OpenAPI spec.
const specPath = "../../../api/openapi.yaml"

// routeKey is the canonical (METHOD, PATH) tuple used for set-diffing.
type routeKey struct {
	method string
	path   string
}

func (r routeKey) String() string { return r.method + " " + r.path }

// TestArch_RouteHasSpecOperation walks every mux.Handle registration
// under internal/identity/ports/http.go + every operation in
// api/openapi.yaml, restricted to the /api/v1/* prefix, and asserts
// the two sets are equal.
func TestArch_RouteHasSpecOperation(t *testing.T) {
	t.Parallel()

	var codeRoutes []routeKey
	for _, p := range codeRoutesPaths {
		got, err := extractCodeRoutes(p)
		if err != nil {
			t.Fatalf("extract code routes from %s: %v", p, err)
		}
		codeRoutes = append(codeRoutes, got...)
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

	sort.Slice(codeOrphans, func(i, j int) bool { return codeOrphans[i].String() < codeOrphans[j].String() })
	sort.Slice(specOrphans, func(i, j int) bool { return specOrphans[i].String() < specOrphans[j].String() })

	if len(codeOrphans) == 0 && len(specOrphans) == 0 {
		return
	}

	t.Logf("OpenAPI ↔ code-route drift detected (per ADR 0050).")
	t.Logf("Total: %d code routes, %d spec operations under /api/v1/*", len(codeSet), len(specSet))
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

// extractCodeRoutes parses http.go via go/parser and pulls every
// `mux.Handle("METHOD /api/v1/...", h)` literal. Restricted to /api/v1/*
// prefix; infrastructure routes (/, /favicon.ico, /docs, /openapi.yaml)
// are out of scope by design.
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
		// Match `<x>.Handle(string-literal, _)` shape — the receiver name
		// doesn't matter (could be `mux`, `m`, etc.) as long as the method
		// name is "Handle" and the first arg is a string literal.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Handle" || len(call.Args) < 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// Strip the surrounding quotes from the Go string literal.
		raw := strings.Trim(lit.Value, "\"`")
		method, p, found := strings.Cut(raw, " ")
		if !found {
			return true
		}
		if !strings.HasPrefix(p, "/api/v1/") {
			return true
		}
		out = append(out, routeKey{method: method, path: p})
		return true
	})
	return out, nil
}

// extractSpecRoutes parses the OpenAPI YAML + extracts every
// (METHOD, /api/v1/...) operation. The spec's top-level `paths:` map
// keys are the paths; each child key is a verb (lowercase).
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
		if !strings.HasPrefix(specPath, "/api/v1/") {
			continue
		}
		// Sibling bounded contexts also live under /api/v1/* (e.g. the
		// Platform module's verification + marketplace + credits routes
		// under /api/v1/platform/{unverified-contacts,marketplace,
		// lead-credits}/...). Identity's drift gate covers identity-
		// owned routes only; each sibling module ships its own scoped
		// route_spec_test.go (per ADR 0050 — drift gates are PER-MODULE
		// since each module owns its corner of the URL space).
		if isPlatformModuleOwnedPath(specPath) {
			continue
		}
		for verb := range ops {
			if _, ok := verbs[strings.ToLower(verb)]; !ok {
				continue
			}
			out = append(out, routeKey{
				method: strings.ToUpper(verb),
				path:   specPath,
			})
		}
	}
	return out, nil
}

// isPlatformModuleOwnedPath reports whether p belongs to the Platform
// bounded context's sub-namespace (ADR 0059). Identity owns the
// `/api/v1/platform/{tenants,persons,impersonation,stats}/...` operator
// surface; Platform owns the listed sub-resources. Each module's own
// route_spec_test.go covers ITS scope; this guard excludes the
// platform-module space from identity's gate.
func isPlatformModuleOwnedPath(p string) bool {
	switch {
	case p == "/api/v1/platform/unverified-contacts",
		strings.HasPrefix(p, "/api/v1/platform/unverified-contacts/"),
		strings.HasPrefix(p, "/api/v1/platform/marketplace/"),
		p == "/api/v1/platform/lead-credits",
		strings.HasPrefix(p, "/api/v1/platform/lead-credits/"):
		return true
	}
	return false
}
