package authn

import "net/http"

// The central X-Tenant-Id route policy (ADR 0039 / ADR 0068 D7).
//
// Default: a route is tenant-plane and HONOURS the header. This table lists
// the route groups where the header cannot apply and is therefore IGNORED
// before anything reads it: no validation, no tenant lookup, no audit line,
// effective tenant = the caller's own. Routes inside an ignored prefix that
// do interpret the header are listed as honouring exceptions.
//
// Per-route reject / path-conflict behaviour is declared where the route is
// registered ([RejectTenantScope], [TenantScopeLifecycle], ...) because it
// is a property of that one route's handler, not of a URL group.
//
// Patterns use net/http.ServeMux syntax, so matching is exactly the router's
// (method, wildcards, most-specific wins) and nothing here is string-prefix
// guessing.
type scopeDecision struct{ ignore bool }

var (
	scopeIgnore = &scopeDecision{ignore: true}
	scopeAware  = &scopeDecision{}
)

// scopePolicy maps route patterns to a decision; see the table below.
var scopePolicy = func() *http.ServeMux {
	m := http.NewServeMux()
	reg := func(pattern string, d *scopeDecision) { m.Handle(pattern, handlerFor(d)) }

	// Self-service identity: always about the caller's own person/session.
	reg("/api/v1/auth/", scopeIgnore)
	// Platform-wide operations: never tenant-plane.
	reg("/api/v1/platform/", scopeIgnore)
	// Tenant onboarding creates a NEW tenant; there is no target tenant.
	reg("POST /api/v1/tenants", scopeIgnore)
	// Platform-wide lookups with no effective tenant: slug resolution and the
	// operator omni-search read across tenants by design.
	reg("GET /api/v1/tenants", scopeIgnore)
	reg("GET /api/v1/tenants/by-slug/{slug}", scopeIgnore)
	reg("GET /api/v1/search", scopeIgnore)

	// Exceptions inside /api/v1/platform/ that interpret the header.
	reg("GET /api/v1/platform/lead-credits/balance", scopeAware)             // reads the scoped tenant's wallet
	reg("POST /api/v1/platform/marketplace/leads/{id}/purchase", scopeAware) // registered with RejectTenantScope
	reg("DELETE /api/v1/platform/tenants/{tenantId}", scopeAware)            // registered with TenantScopePathOnly
	return m
}()

type decisionHandler struct{ d *scopeDecision }

func (decisionHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func handlerFor(d *scopeDecision) http.Handler { return decisionHandler{d: d} }

// scopeIgnoredOn reports whether the central policy says the header is
// ignored for this request.
func scopeIgnoredOn(r *http.Request) bool {
	h, _ := scopePolicy.Handler(r)
	dh, ok := h.(decisionHandler)
	return ok && dh.d.ignore
}
