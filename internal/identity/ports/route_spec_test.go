// route_spec_test.go — TestArch_ guard preventing drift between the
// Identity Go HTTP route table and api/openapi.yaml.
//
// Per ADR 0050: the OpenAPI spec is the canonical contract (ADR 0046);
// the Go routes MUST match it exactly under identity-owned prefixes.
// This arch test is the gate.
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
// Scope: identity-owned prefixes only. Sibling bounded contexts
// (Platform, Inventory, CRM) ship their own per-module
// route_spec_test.go (ADR 0050 — drift gates are PER-MODULE since each
// module owns its corner of the URL space). Infrastructure routes
// (`GET /`, `GET /favicon.ico`, `GET /docs`, `GET /openapi.yaml`)
// live OUTSIDE the spec on purpose — they're cross-cutting, not
// product API surface — and don't appear under any owned prefix.
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
// code-side truth for the IDENTITY drift gate. Other modules ship
// their OWN route_spec_test.go (platform, inventory, crm) — drift
// gates are PER-MODULE per ADR 0050 since each module owns its
// corner of the URL space.
var codeRoutesPaths = []string{
	"http.go",
}

// specPath is the canonical OpenAPI spec.
const specPath = "../../../api/openapi.yaml"

// identityOwnedPrefixes are the URL prefixes THIS module owns. Each
// prefix is a leading substring (treated as a prefix that ALSO matches
// the bare prefix itself when the spec/code uses it as a complete path).
// Mirror of platform's platformOwnedPrefixes shape (ADR 0050 canon —
// positive prefix matching, not negative filtering of other modules).
//
// Adding a new identity-owned sub-resource means appending its prefix
// here AND adding the spec entry — the drift gate then enforces both
// directions for the new namespace.
var identityOwnedPrefixes = []string{
	"/api/v1/auth",
	"/api/v1/sessions",
	"/api/v1/tenants",
	"/api/v1/users",
	"/api/v1/roles",
	"/api/v1/role-hierarchy",
	"/api/v1/permissions",
	"/api/v1/permission-requests",
	"/api/v1/search",
	// Identity-owned slices under the shared /platform/ namespace.
	// Sibling slices (unverified-contacts, marketplace, lead-credits)
	// belong to the Platform module and are covered by its own gate.
	"/api/v1/platform/tenants",
	"/api/v1/platform/persons",
	"/api/v1/platform/impersonation",
	"/api/v1/platform/stats",
}

// matchesOwnedPrefix reports whether p falls under one of the
// identityOwnedPrefixes namespaces (treats "/foo" as covering "/foo"
// + "/foo/...").
func matchesOwnedPrefix(p string) bool {
	for _, pref := range identityOwnedPrefixes {
		if p == pref || strings.HasPrefix(p, pref+"/") || strings.HasPrefix(p, pref) {
			return true
		}
	}
	return false
}

// routeKey is the canonical (METHOD, PATH) tuple used for set-diffing.
type routeKey struct {
	method string
	path   string
}

func (r routeKey) String() string { return r.method + " " + r.path }

// TestArch_RouteHasSpecOperation walks every mux.Handle registration
// under internal/identity/ports/http.go + every operation in
// api/openapi.yaml, restricted to identity-owned prefixes, and asserts
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

	t.Logf("OpenAPI ↔ Identity code-route drift detected (per ADR 0050).")
	t.Logf("Total: %d code routes, %d spec operations under identity-owned prefixes %v",
		len(codeSet), len(specSet), identityOwnedPrefixes)
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
// `mux.Handle("METHOD /api/v1/...", h)` literal. Restricted to
// identity-owned prefixes; infrastructure routes (/, /favicon.ico,
// /docs, /openapi.yaml) are out of scope by design.
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
		if !matchesOwnedPrefix(p) {
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
		if !matchesOwnedPrefix(specPath) {
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
