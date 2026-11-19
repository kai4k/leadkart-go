package authn

import (
	"net/http"
	"net/url"
)

// The central X-Tenant-Id route policy (ADR 0039 / ADR 0068 D7).
//
// A route's scope policy is declared in exactly one of two places:
//
//   - a registration MARKER on the route ([RejectTenantScope],
//     [TenantScopeMatchesPath], [TenantScopeLifecycle], [TenantScopePathOnly],
//     [TenantScopeWritable], [HonourTenantScope]); or
//   - this table, for whole route GROUPS where the header cannot apply and is
//     therefore IGNORED: no validation, no tenant lookup, no audit line,
//     effective tenant = the caller's own.
//
// A marker always wins: the table's IGNORE only applies to a route that
// carries no marker. A route with neither HONOURS the header (tenant plane),
// read-only unless it is marked writable.
//
// Patterns use net/http.ServeMux syntax, so matching is exactly the router's
// (method, wildcards, most-specific wins) and nothing here is string-prefix
// guessing.
var scopeIgnoreTable = func() *http.ServeMux {
	m := http.NewServeMux()
	ignore := func(pattern string) { m.Handle(pattern, ignoreMarker{}) }

	// Self-service identity: always about the caller's own person/session.
	ignore("/api/v1/auth/")
	// Platform-wide operations: never tenant-plane. Routes inside this group
	// that interpret the header say so with a marker at registration.
	ignore("/api/v1/platform/")
	// The caller's own notification inbox (recipient = the caller's membership).
	ignore("/api/v1/notifications")
	ignore("/api/v1/notifications/")
	// Tenant onboarding creates a NEW tenant; there is no target tenant.
	ignore("POST /api/v1/tenants")
	// Platform-wide lookups with no effective tenant: slug resolution and the
	// operator omni-search read across tenants by design.
	ignore("GET /api/v1/tenants")
	ignore("GET /api/v1/tenants/by-slug/{slug}")
	ignore("GET /api/v1/search")
	return m
}()

type ignoreMarker struct{}

func (ignoreMarker) ServeHTTP(http.ResponseWriter, *http.Request) {}

// scopeIgnoredOn reports whether the central table says the header is ignored
// for this request (a route marker may still override it).
func scopeIgnoredOn(r *http.Request) bool {
	h, _ := scopeIgnoreTable.Handler(r)
	_, ok := h.(ignoreMarker)
	return ok
}

// ScopeIgnoredByTable reports whether the central table ignores X-Tenant-Id for
// method + path (a concrete path; wildcard segments may be any literal). A
// route marker may still override the table (see the package policy comment);
// this exists so the architecture test can classify every registered route.
func ScopeIgnoredByTable(method, path string) bool {
	return scopeIgnoredOn(&http.Request{Method: method, URL: &url.URL{Path: path}})
}
