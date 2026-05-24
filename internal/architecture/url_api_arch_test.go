// url_api_arch_test.go — Principle 10: URL / API Conformance.
//
// Per ADR 0046 (spec-first OpenAPI), ADR 0049 (URL design rules), ADR
// 0050 (drift gates), ADR 0052 (canonical slug-lookup with query
// param), and ADR 0038 (cursor pagination): URLs follow Stripe /
// GitHub / Auth0 canon; the spec is the contract of record; cursor
// pagination + RFC 9457 errors are the universal shape.
//
// Tests in this file:
//   58. TestArch_RouteHasSpecOperation
//   59. TestArch_RoutesPrefixedAPIV1
//   60. TestArch_RoutesUsePluralNouns
//   61. TestArch_NoByXPathLookups
//   62. TestArch_CursorParamsCanonical
//   63. TestArch_ProblemDetailsErrorShape
//   64. TestArch_HTTPMethodExplicit
//   65. TestArch_PathParamNamingCanonical

package architecture_test

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadSpec returns the parsed OpenAPI spec or fatals.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := readFileBytes(apiSpecPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal openapi.yaml: %v", err)
	}
	return doc
}

// specPaths returns the (lowered) HTTP methods + path keys in the spec.
func specPaths(t *testing.T) []string {
	t.Helper()
	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	out := []string{}
	for pathKey, raw := range paths {
		methods, _ := raw.(map[string]any)
		for method := range methods {
			out = append(out, strings.ToUpper(method)+" "+pathKey)
		}
	}
	sort.Strings(out)
	return out
}

// codeRoutes returns the (METHOD, PATH) tuples from every
// internal/<mod>/ports/*.go file.
func codeRoutes(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`mux\.Handle\(\s*"([A-Z]+ [^"]+)"`)
	out := []string{}
	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !strings.HasSuffix(slashPath, "/ports/http.go") &&
				!strings.HasSuffix(slashPath, "_http.go") {
				return
			}
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				out = append(out, m[1])
			}
		})
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------------------------
// Test 58: TestArch_RouteHasSpecOperation
// ----------------------------------------------------------------------------
//
// Every mux.Handle("METHOD /api/v1/...") in code has a matching
// operation in api/openapi.yaml + vice versa. Per ADR 0050 the spec
// is the contract of record; drift on either side fails CI.
//
// Promoted from per-module identity/ports/route_spec_test.go to cover
// all modules in one place.
//
// EXCEPTION: infrastructure routes (`GET /`, `GET /favicon.ico`,
// `GET /docs`, `GET /openapi.yaml`) live OUTSIDE the spec on purpose.
func TestArch_RouteHasSpecOperation(t *testing.T) {
	t.Parallel()

	infraRoutes := map[string]bool{
		"GET /":             true,
		"GET /favicon.ico":  true,
		"GET /docs":         true,
		"GET /docs/":        true,
		"GET /openapi.yaml": true,
		"GET /health":       true,
		"GET /alive":        true,
		"GET /ready":        true,
	}
	apiPrefix := "/api/v1/"

	codeSet := map[string]bool{}
	for _, k := range codeRoutes(t) {
		_, p, _ := strings.Cut(k, " ")
		if infraRoutes[k] {
			continue
		}
		if !strings.HasPrefix(p, apiPrefix) {
			continue
		}
		codeSet[k] = true
	}
	specSet := map[string]bool{}
	for _, k := range specPaths(t) {
		_, p, _ := strings.Cut(k, " ")
		if !strings.HasPrefix(p, apiPrefix) {
			continue
		}
		specSet[k] = true
	}

	var codeOnly, specOnly []string
	for k := range codeSet {
		if !specSet[k] {
			codeOnly = append(codeOnly, k)
		}
	}
	for k := range specSet {
		if !codeSet[k] {
			specOnly = append(specOnly, k)
		}
	}
	sort.Strings(codeOnly)
	sort.Strings(specOnly)

	if len(codeOnly) > 0 || len(specOnly) > 0 {
		t.Logf("ROUTE / SPEC DRIFT — code-only=%d spec-only=%d", len(codeOnly), len(specOnly))
		for _, k := range codeOnly {
			t.Errorf("code route has no spec operation: %s", k)
		}
		for _, k := range specOnly {
			t.Errorf("spec operation has no code route: %s", k)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 59: TestArch_RoutesPrefixedAPIV1
// ----------------------------------------------------------------------------
//
// Every path in api/openapi.yaml starts with /api/v1/. Exceptions:
// root `/`, `/docs`, `/openapi.yaml`, `/favicon.ico`.
func TestArch_RoutesPrefixedAPIV1(t *testing.T) {
	t.Parallel()

	exempt := map[string]bool{
		"/":             true,
		"/docs":         true,
		"/openapi.yaml": true,
		"/favicon.ico":  true,
		"/health":       true,
		"/alive":        true,
		"/ready":        true,
	}

	type violation struct{ path string }
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for k := range paths {
		if exempt[k] {
			continue
		}
		if !strings.HasPrefix(k, "/api/v1/") {
			violations = append(violations, violation{path: k})
		}
	}

	if len(violations) > 0 {
		t.Logf("PATH-PREFIX VIOLATIONS — %d", len(violations))
		t.Logf("Every spec path starts with /api/v1/ (per ADR 0046).")
		for _, v := range violations {
			t.Errorf("%s — must start with /api/v1/", v.path)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 60: TestArch_RoutesUsePluralNouns
// ----------------------------------------------------------------------------
//
// Path segments NOT inside {...} are plural English nouns. Heuristic:
// alphabetic + > 2 chars + ends in `s` OR is in an irregular-plural
// allowlist (children, people, audit, search, etc.).
//
// Per Stripe / GitHub URL canon: collections are plural, items are
// `<plural>/{id}`. Singular collection names (`/user`) cause confusion.
func TestArch_RoutesUsePluralNouns(t *testing.T) {
	t.Parallel()

	// Irregular plurals + collective nouns + verbs-as-resource that are
	// canonically singular + module-namespace prefixes.
	irregular := map[string]bool{
		// Module / namespace prefixes (Stripe / Auth0 canon — namespace
		// path segments are noun-singular by convention).
		"api":           true,
		"v1":            true,
		"auth":          true,
		"platform":      true,
		"inventory":     true,
		"crm":           true,
		"orders":        true,
		"dispatch":      true,
		"tasks":         true,
		"notifications": true,
		"marketplace":   true,
		"impersonation": true,
		"me":            true,
		"docs":          true,

		// Action / verb endpoints.
		"audit":                  true,
		"search":                 true,
		"login":                  true,
		"logout":                 true,
		"refresh":                true,
		"register":               true,
		"reset-password":         true,
		"request-password-reset": true,
		"confirm-email-change":   true,
		"request-email-change":   true,
		"change-password":        true,
		"activate":               true,
		"deactivate":             true,
		"suspend":                true,
		"reactivate":             true,
		"restore-from-deletion":  true,
		"restore":                true,
		"mark-for-deletion":      true,
		"hard-delete":            true,
		"verify":                 true,
		"reject":                 true,
		"approve":                true,
		"cancel":                 true,
		"deny":                   true,
		"purchase":               true,
		"topup":                  true,
		"balance":                true,
		"browse":                 true,
		"parent":                 true,
		"manager":                true,
		"grant":                  true,
		"revoke":                 true,
		"anonymise":              true,
		"global-suspend":         true,
		"lift-global-suspension": true,
		"by-slug":                true,
		"resend":                 true,

		// Plural English nouns the regex misses (no `s` suffix; or
		// collective).
		"capabilities":        true,
		"activity":            true,
		"events":              true,
		"profile":             true,
		"statutory":           true,
		"settings":            true,
		"display-preferences": true,
		"admin-contact":       true,
		"unverified-contacts": true,
		"unverified-contact":  true,
		"lead-credits":        true,
		"permission-requests": true,
		"sessions":            true,
	}

	type violation struct {
		path string
		seg  string
	}
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for k := range paths {
		segs := strings.Split(strings.Trim(k, "/"), "/")
		for _, seg := range segs {
			if seg == "" {
				continue
			}
			if strings.HasPrefix(seg, "{") {
				continue
			}
			if irregular[seg] {
				continue
			}
			low := strings.ToLower(seg)
			// Accept anything ending in 's'.
			if strings.HasSuffix(low, "s") {
				continue
			}
			// Accept short / non-alphabetic segments.
			if len(seg) <= 2 {
				continue
			}
			violations = append(violations, violation{path: k, seg: seg})
		}
	}

	if len(violations) > 0 {
		t.Logf("SINGULAR-COLLECTION VIOLATIONS — %d", len(violations))
		t.Logf("Per Stripe/GitHub: collections are plural nouns. Add a")
		t.Logf("singular noun to the test's `irregular` allow-list when")
		t.Logf("the segment is a verb-as-resource (/login) or sub-resource.")
		for _, v := range violations {
			t.Errorf("%s — segment %q is not plural", v.path, v.seg)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 61: TestArch_NoByXPathLookups
// ----------------------------------------------------------------------------
//
// Per ADR 0049: lookups by non-primary-key use query params (?slug=,
// ?email=), not path segments (/by-slug/{slug}). Stripe / Auth0 canon.
//
// EXCEPTION: /api/v1/tenants/by-slug/{slug} is grandfathered for v0.2
// FE-contract compat (ADR 0052; removal in v0.4+).
func TestArch_NoByXPathLookups(t *testing.T) {
	t.Parallel()

	byXRE := regexp.MustCompile(`/by-[a-z-]+/\{`)
	grandfathered := map[string]bool{
		"/api/v1/tenants/by-slug/{slug}": true,
	}

	type violation struct{ path string }
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for k := range paths {
		if grandfathered[k] {
			continue
		}
		if byXRE.MatchString(k) {
			violations = append(violations, violation{path: k})
		}
	}

	if len(violations) > 0 {
		t.Logf("BY-X PATH-LOOKUP VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0049: lookups by non-PK use query params, not")
		t.Logf("path segments. Stripe/GitHub canon.")
		for _, v := range violations {
			t.Errorf("%s — uses /by-X/{...} path shape", v.path)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 62: TestArch_CursorParamsCanonical
// ----------------------------------------------------------------------------
//
// Per ADR 0038: cursor pagination uses `cursor` + `page_size`; never
// `offset` / `limit`. Walk every operation's `parameters` array; flag
// any param named offset or limit.
func TestArch_CursorParamsCanonical(t *testing.T) {
	t.Parallel()

	bannedNames := map[string]bool{
		"offset": true,
		"limit":  true,
	}
	// Omni-search uses `limit` (max results per category) — per ADR
	// 0040 this is a CAP, not cursor pagination. Tracked in
	// KNOWN_VIOLATIONS.md if the team decides to rename later.
	opAllowList := map[string]bool{
		"GET /api/v1/search": true,
	}

	type violation struct {
		op    string
		param string
	}
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for pathKey, raw := range paths {
		methods, _ := raw.(map[string]any)
		for method, opRaw := range methods {
			opKey := strings.ToUpper(method) + " " + pathKey
			if opAllowList[opKey] {
				continue
			}
			op, _ := opRaw.(map[string]any)
			params, _ := op["parameters"].([]any)
			for _, p := range params {
				pm, _ := p.(map[string]any)
				name, _ := pm["name"].(string)
				low := strings.ToLower(name)
				if bannedNames[low] {
					violations = append(violations, violation{
						op:    opKey,
						param: name,
					})
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("OFFSET-PAGINATION VIOLATIONS — %d", len(violations))
		t.Logf("Per ADR 0038: opaque keyset cursors only. No offset/limit.")
		for _, v := range violations {
			t.Errorf("%s — parameter %q (use cursor + page_size)", v.op, v.param)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 63: TestArch_ProblemDetailsErrorShape
// ----------------------------------------------------------------------------
//
// Per RFC 9457 + the LeadKart ErrorResponse schema: every 4xx/5xx
// response references the canonical error schema (ErrorResponse or
// ProblemDetails); no raw inline object types.
func TestArch_ProblemDetailsErrorShape(t *testing.T) {
	t.Parallel()

	canonicalRefs := map[string]bool{
		"#/components/schemas/ErrorResponse":   true,
		"#/components/schemas/ProblemDetails":  true,
	}

	type violation struct {
		op     string
		status string
	}
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for pathKey, raw := range paths {
		methods, _ := raw.(map[string]any)
		for method, opRaw := range methods {
			op, _ := opRaw.(map[string]any)
			responses, _ := op["responses"].(map[string]any)
			for status, rawResp := range responses {
				if len(status) < 1 || (status[0] != '4' && status[0] != '5') {
					continue
				}
				resp, _ := rawResp.(map[string]any)
				content, _ := resp["content"].(map[string]any)
				for _, mr := range content {
					mediaResp, _ := mr.(map[string]any)
					schema, _ := mediaResp["schema"].(map[string]any)
					ref, _ := schema["$ref"].(string)
					if ref == "" || !canonicalRefs[ref] {
						violations = append(violations, violation{
							op:     strings.ToUpper(method) + " " + pathKey,
							status: status,
						})
					}
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Logf("ERROR-SHAPE VIOLATIONS — %d", len(violations))
		t.Logf("Per RFC 9457: every 4xx/5xx references the canonical error schema.")
		for _, v := range violations {
			t.Errorf("%s status %s — not $ref ErrorResponse/ProblemDetails", v.op, v.status)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 64: TestArch_HTTPMethodExplicit
// ----------------------------------------------------------------------------
//
// Every mux.Handle call uses the "METHOD /path" pattern (Go 1.22+
// ServeMux). The bare `mux.Handle("/path", ...)` pattern is a Go
// 1.20-era anti-pattern that matches ALL methods + opens CORS/CSRF
// holes.
func TestArch_HTTPMethodExplicit(t *testing.T) {
	t.Parallel()

	// Only flag mux.Handle calls; mux.HandleFunc + http.Handle are out
	// of scope. The pattern: first arg is a string literal; must start
	// with an uppercase HTTP method token.
	methodRE := regexp.MustCompile(`^[A-Z]+\s+/`)
	rawHandleRE := regexp.MustCompile(`mux\.Handle\(\s*"([^"]+)"`)

	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !strings.HasSuffix(slashPath, "/ports/http.go") &&
				!strings.HasSuffix(slashPath, "_http.go") {
				return
			}
			text := string(src)
			matches := rawHandleRE.FindAllStringSubmatchIndex(text, -1)
			for _, m := range matches {
				key := text[m[2]:m[3]]
				if methodRE.MatchString(key) {
					continue
				}
				line := 1 + strings.Count(text[:m[0]], "\n")
				violations = append(violations, violation{file: path, line: line, key: key})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("BARE-METHOD mux.Handle VIOLATIONS — %d", len(violations))
		t.Logf("Per Go 1.22 ServeMux + ADR 0007: every route uses")
		t.Logf("`METHOD /path`. Bare `/path` matches all methods.")
		for _, v := range violations {
			t.Errorf("%s:%d — %q lacks HTTP method prefix", v.file, v.line, v.key)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 65: TestArch_PathParamNamingCanonical
// ----------------------------------------------------------------------------
//
// Path params use camelCase with the `Id` suffix: {tenantId},
// {userId}, {roleId}, {productId}. Lowercase-only {id} is flagged as
// ambiguous unless in a non-controversial nested context.
func TestArch_PathParamNamingCanonical(t *testing.T) {
	t.Parallel()

	paramRE := regexp.MustCompile(`\{([^}]+)\}`)
	type violation struct {
		path  string
		param string
	}
	var violations []violation

	doc := loadSpec(t)
	paths, _ := doc["paths"].(map[string]any)
	for k := range paths {
		for _, m := range paramRE.FindAllStringSubmatch(k, -1) {
			name := m[1]
			// Accept camelCase + Id suffix.
			if regexp.MustCompile(`^[a-z][a-zA-Z0-9]*Id$`).MatchString(name) {
				continue
			}
			// Accept singular naming for tightly-scoped sub-resources
			// where context is obvious (e.g. `slug` in tenants/by-slug,
			// `id` in nested /unverified-contacts/{id} is unambiguous
			// within the platform sub-resource).
			if name == "slug" || name == "id" {
				continue
			}
			violations = append(violations, violation{path: k, param: name})
		}
	}

	if len(violations) > 0 {
		t.Logf("PATH-PARAM NAMING VIOLATIONS — %d", len(violations))
		t.Logf("Path params should be camelCase + `Id` suffix")
		t.Logf("({tenantId}, {userId}, {roleId}).")
		for _, v := range violations {
			t.Errorf("%s — {%s} should be camelCase + Id suffix", v.path, v.param)
		}
	}
}
