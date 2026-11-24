// scope_classification_arch_test.go — ADR 0068 D7: every registered route has
// ONE declared X-Tenant-Id scope policy, and a route that honours the header
// is authorised by a permission, never by authentication or platform-tenant
// membership alone.
package architecture_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// scopeWritableRoutes is the EXPLICIT set of routes that may write under a
// foreign X-Tenant-Id (authn.TenantScopeWritable). Adding one is a deliberate,
// reviewed decision: every other honoured mutation is 403
// tenant_scope_read_only under a foreign header, which is what keeps an
// operator's own membership id out of another tenant's rows.
var scopeWritableRoutes = []string{
	"DELETE /api/v1/users/{userId}/manager",
	"DELETE /api/v1/users/{userId}/roles/{roleId}",
	"PATCH /api/v1/roles/{roleId}",
	"PATCH /api/v1/roles/{roleId}/parent",
	"PATCH /api/v1/users/{userId}/permission-overrides",
	"PATCH /api/v1/users/{userId}/profile",
	"POST /api/v1/roles",
	"POST /api/v1/roles/{roleId}/deactivate",
	"POST /api/v1/roles/{roleId}/permissions/grant",
	"POST /api/v1/roles/{roleId}/permissions/revoke",
	"POST /api/v1/roles/{roleId}/reactivate",
	"POST /api/v1/users",
	"POST /api/v1/users/{userId}/deactivate",
	"POST /api/v1/users/{userId}/reactivate",
	"POST /api/v1/users/{userId}/roles",
	"PUT /api/v1/roles/{roleId}/permissions",
	"PUT /api/v1/users/{userId}/manager",
}

const (
	scopeClassIgnore = "IGNORE"
	scopeClassReject = "REJECT"
	scopeClassPath   = "path-tenant"
	scopeClassHonour = "honour"
)

type classifiedRoute struct {
	reg      routeReg
	class    string
	writable bool
	problems []string
}

var scopeWildcardRE = regexp.MustCompile(`\{[^}]+\}`)

// classifyScopeRoute classifies one scanned route from its wrapper tags.
// Routes outside /api/v1/ (cmd/api's root, docs and openapi helpers) carry no
// tenant scope and are not classified.
func classifyScopeRoute(reg routeReg) (classifiedRoute, bool) {
	if !strings.HasPrefix(reg.Path, "/api/v1/") {
		return classifiedRoute{}, false
	}
	concrete := scopeWildcardRE.ReplaceAllString(reg.Path, "x")
	marked := reg.has(tagReject) || reg.has(tagPath) || reg.has(tagWritable) || reg.has(tagHonour)
	r := classifiedRoute{reg: reg, writable: reg.has(tagWritable)}
	switch {
	case reg.has(tagReject):
		r.class = scopeClassReject
	case reg.has(tagPath):
		r.class = scopeClassPath
	case authn.ScopeIgnoredByTable(reg.Method, concrete) && !marked:
		r.class = scopeClassIgnore
	default:
		r.class = scopeClassHonour
	}
	r.problems = markersBeforeGate(reg)
	if r.class == scopeClassHonour && !reg.has(tagAuthz) {
		r.problems = append(r.problems, "honours X-Tenant-Id but carries no permission gate evaluated in the scoped tenant: add one, or declare the route IGNORE (authn scope_policy table) / REJECT (authn.RejectTenantScope)")
	}
	if r.writable && r.class != scopeClassHonour {
		r.problems = append(r.problems, "authn.TenantScopeWritable on a route that is not an honoured tenant-plane route ("+r.class+")")
	}
	return r, true
}

// TestArch_EveryRouteIsScopeClassified fails when a registered route is not
// exactly one of: IGNORE group, REJECT, path-tenant, or honour + permission
// gate evaluated in the scoped tenant; when a scope marker is wrapped inside
// the auth gate (it would run after the scope is applied and do nothing); when
// a registration cannot be read by the shared scanner (see route_scan_test.go);
// or when the set of routes that may write under a foreign X-Tenant-Id differs
// from scopeWritableRoutes.
//
// Scope: production — every non-test file under each module's ports/ and cmd/api.
//
// Negative coverage: TestRouteScanner_* (route_scan_test.go) run the shared
// scanner and verdict over synthetic offenders, and
// TestScopeClassifier_FlagsMisclassifiedRoutes covers this classifier.
//
// arch-test:no-negative-fixture (RED proof: wrap any honoured identity route in `auth(` only, or move a marker inside its gate → this test fails; synthetic offenders in the TestRouteScanner_* and TestScopeClassifier_* tests)
func TestArch_EveryRouteIsScopeClassified(t *testing.T) {
	t.Parallel()
	routes, offences := scanAllRoutes(t)
	failOnOffences(t, offences)
	var writable []string
	total := 0
	counts := map[string]int{}
	for _, reg := range routes {
		r, ok := classifyScopeRoute(reg)
		if !ok {
			continue
		}
		total++
		counts[r.class]++
		if r.writable {
			writable = append(writable, reg.Pattern)
		}
		for _, p := range r.problems {
			t.Errorf("%s:%d %s [%s] — %s", reg.File, reg.Line, reg.Pattern, r.class, p)
		}
	}
	if total == 0 || counts[scopeClassHonour] == 0 || counts[scopeClassIgnore] == 0 || counts[scopeClassReject] == 0 || counts[scopeClassPath] == 0 {
		t.Fatalf("classifier found no routes in some class (total=%d %v): the scan is broken", total, counts)
	}
	slices.Sort(writable)
	want := slices.Clone(scopeWritableRoutes)
	slices.Sort(want)
	if !slices.Equal(writable, want) {
		t.Errorf("routes writable under a foreign X-Tenant-Id changed.\n got:  %v\n want: %v\nAdding one is a deliberate decision: update scopeWritableRoutes with the reason in the commit.", writable, want)
	}
}

// TestScopeClassifier_FlagsMisclassifiedRoutes is the negative proof for the
// classifier half of TestArch_EveryRouteIsScopeClassified.
func TestScopeClassifier_FlagsMisclassifiedRoutes(t *testing.T) {
	t.Parallel()
	const src = `package ports
import ("net/http"; "github.com/leadkart/leadkart-go/internal/identity/ports/authn")
func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	auth := authn.RequireFreshStamp(v, s)
	read := authn.RequirePermission(v, s, p.Orders.Read)
	manage := authn.RequirePermission(v, s, p.Orders.Manage)
	gate := authn.RequirePlatformGate(v, s)
	mux.Handle("GET /api/v1/orders/authonly", auth(h))
	mux.Handle("POST /api/v1/orders/ok", manage(h))
	mux.Handle("GET /api/v1/orders/ok", read(h))
	mux.Handle("GET /api/v1/platform/stats-ok", gate(p.Platform.TenantsView)(h))
	mux.Handle("GET /api/v1/auth/sessions", auth(h))
	mux.Handle("POST /api/v1/permission-requests", authn.RejectTenantScope(auth(h)))
	mux.Handle("POST /api/v1/orders/writable-ignored", authn.TenantScopeWritable(authn.RejectTenantScope(manage(h))))
	mux.Handle("GET /api/v1/orders/marker-inside", read(authn.RejectTenantScope(h)))
}`
	routes, off := scanRouteSource("internal/x/ports/http.go", []byte(src))
	if len(off) != 0 {
		t.Fatalf("offences: %v", off)
	}
	got := map[string][]string{}
	for _, reg := range routes {
		r, _ := classifyScopeRoute(reg)
		got[reg.Pattern] = r.problems
	}
	for _, key := range []string{
		"GET /api/v1/orders/authonly",          // honoured, authentication only
		"POST /api/v1/orders/writable-ignored", // writable on a REJECT route
		"GET /api/v1/orders/marker-inside",     // marker inside the gate
	} {
		if len(got[key]) == 0 {
			t.Errorf("%s must be flagged", key)
		}
	}
	for _, key := range []string{
		"POST /api/v1/orders/ok", "GET /api/v1/orders/ok", "GET /api/v1/platform/stats-ok",
		"GET /api/v1/auth/sessions", "POST /api/v1/permission-requests",
	} {
		if p, ok := got[key]; !ok || len(p) != 0 {
			t.Errorf("%s must be classified without problems, got %v (present=%v)", key, p, ok)
		}
	}
}
