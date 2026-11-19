// authz_arch_test.go — ADR 0068: every route is authorised, not merely
// authenticated.
package architecture_test

import (
	"slices"
	"testing"
)

// selfServiceRoutes is the PINNED list of routes with no permission gate: they
// are public (credential exchange, emailed-token flows, health/docs), act
// solely on the caller's own Person / session / inbox, or carry handler-inline
// authorisation mandated by an ADR. It is asserted EXACTLY: a route without an
// authorising gate that is not listed fails, and an entry that is not
// registered, or that does carry an authorising gate, fails. Additions are a
// deliberate, reviewed decision with a reason.
var selfServiceRoutes = map[string]string{
	// Public: credential exchange and emailed-token flows (ADR 0011, ADR 0053).
	"POST /api/v1/auth/login":                  "public: credential exchange",
	"POST /api/v1/auth/refresh":                "public: refresh-token exchange",
	"POST /api/v1/auth/logout":                 "public: revokes the presented refresh family",
	"POST /api/v1/auth/request-password-reset": "public: emailed-token flow",
	"POST /api/v1/auth/reset-password":         "public: emailed-token flow",
	"POST /api/v1/auth/confirm-email-change":   "public: emailed-token flow",
	// Self-service identity: the caller's own credential, sessions, activity.
	"POST /api/v1/auth/change-password":       "self: own credential (ADR 0053)",
	"POST /api/v1/auth/request-email-change":  "self: own email",
	"GET /api/v1/auth/sessions":               "self: own refresh families",
	"DELETE /api/v1/auth/sessions/{familyId}": "self: own refresh family (owner-checked in command)",
	"DELETE /api/v1/auth/sessions":            "self: own refresh families",
	"GET /api/v1/auth/me/capabilities":        "self: projection of the caller's own token",
	"GET /api/v1/auth/me/activity":            "self: own activity",
	// Permission requests (REJECT scope): ADR 0055 section 8, handler-inline authority.
	"POST /api/v1/permission-requests":                     "ADR 0055 §8: any caller may submit",
	"GET /api/v1/permission-requests":                      "ADR 0055 §8: own and approver-visible requests, filtered in the handler",
	"GET /api/v1/permission-requests/{requestId}":          "ADR 0055 §8: requester or approver, checked in the handler",
	"POST /api/v1/permission-requests/{requestId}/approve": "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/deny":    "ADR 0055 §8: handler-inline manager-or-platform",
	"POST /api/v1/permission-requests/{requestId}/cancel":  "ADR 0055 §8: handler-inline requester-only",
	// Infrastructure served by cmd/api: no tenant data.
	"GET /{$}":          "infra: root redirect to docs",
	"GET /favicon.ico":  "infra",
	"GET /openapi.yaml": "infra: the spec",
	"GET /docs":         "infra: API reference",
	"GET /docs/":        "infra: API reference assets",
}

// TestArch_MutatingRoutesRequireAuthorization (name kept: ADR 0068 cites it)
// fails when ANY registered route, GETs and the IGNORE group included, carries
// no authorising gate (authn.RequirePermission / RequireAnyPermission /
// RequireTenantPermission / RequirePermissionOrSelf / RequirePlatformGate)
// and is not in selfServiceRoutes; when a registration cannot be read by the
// shared scanner; or when selfServiceRoutes is stale. Authentication or
// platform-tenant membership alone is never authorisation.
//
// Scope: production — every non-test file under each module's ports/ and cmd/api.
//
// arch-test:no-negative-fixture (RED proof: wrap any identity route in `auth(` → this test fails; synthetic offenders in TestRouteScanner_VerdictRejectsUnauthorisedAndMisplacedGates)
func TestArch_MutatingRoutesRequireAuthorization(t *testing.T) {
	t.Parallel()
	routes, offences := scanAllRoutes(t)
	failOnOffences(t, offences)
	if len(routes) == 0 {
		t.Fatal("scanner found no routes")
	}
	registered := map[string]routeReg{}
	var unlisted []string
	for _, reg := range routes {
		registered[reg.Pattern] = reg
		for _, p := range routeVerdict(reg, selfServiceRoutes) {
			t.Errorf("%s:%d %s — %s", reg.File, reg.Line, reg.Pattern, p)
		}
		if _, ok := selfServiceRoutes[reg.Pattern]; ok && reg.has(tagAuthz) {
			t.Errorf("selfServiceRoutes entry %q carries an authorising gate — remove it from the list", reg.Pattern)
		}
		if !reg.has(tagAuthz) {
			unlisted = append(unlisted, reg.Pattern)
		}
	}
	for key := range selfServiceRoutes {
		if _, ok := registered[key]; !ok {
			t.Errorf("selfServiceRoutes entry %q is not registered by any module — remove it", key)
		}
	}
	slices.Sort(unlisted)
	t.Logf("routes without an authorising gate: %q", unlisted)
}
