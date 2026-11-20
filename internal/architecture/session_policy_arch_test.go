// session_policy_arch_test.go — plan D8 (forced password change) and D9
// (impersonation reach): the per-route session policies are declared once, in
// authn's session policy, and these gates prove them against EVERY registered
// route, not against a hand-kept list of routes.
//
// The route list comes from the shared scanner (route_scan_test.go); the
// behaviour comes from serving each scanned route through the real route
// registrations of every module (their AddRoutes, zero-value applications: a
// request that passes its gate may fail inside the handler, which only means the
// gate let it through).
package architecture_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/httpmw"
	crmapp "github.com/leadkart/leadkart-go/internal/crm/app"
	crmports "github.com/leadkart/leadkart-go/internal/crm/ports"
	dispatchapp "github.com/leadkart/leadkart-go/internal/dispatch/app"
	dispatchports "github.com/leadkart/leadkart-go/internal/dispatch/ports"
	identityapp "github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	identityports "github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	inventoryapp "github.com/leadkart/leadkart-go/internal/inventory/app"
	inventoryports "github.com/leadkart/leadkart-go/internal/inventory/ports"
	notificationsapp "github.com/leadkart/leadkart-go/internal/notifications/app"
	notificationsports "github.com/leadkart/leadkart-go/internal/notifications/ports"
	ordersapp "github.com/leadkart/leadkart-go/internal/orders/app"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
	platformports "github.com/leadkart/leadkart-go/internal/platform/ports"
	tasksapp "github.com/leadkart/leadkart-go/internal/tasks/app"
	tasksports "github.com/leadkart/leadkart-go/internal/tasks/ports"
)

// anonymousAPIRoutes is the PINNED set of /api/v1 routes that never pass through
// the stamp check (no bearer token): credential exchange and the emailed-token
// flows. They are the only routes a must_change_password token is not judged on,
// because they do not look at a token at all.
var anonymousAPIRoutes = []string{
	"POST /api/v1/auth/confirm-email-change",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
	"POST /api/v1/auth/refresh",
	"POST /api/v1/auth/request-password-reset",
	"POST /api/v1/auth/reset-password",
}

// tokenFake serves one claims set for the bearer "tok".
type tokenFake struct{ claims *jwt.Claims }

func (v tokenFake) Verify(tok string) (*jwt.Claims, error) {
	if tok != "tok" {
		return nil, errors.New("unknown token")
	}
	return v.claims, nil
}

// liveValidator vouches for every stamp and every impersonation session.
type liveValidator struct{}

func (liveValidator) IsFresh(context.Context, string, string) (bool, error) { return true, nil }

// allRoutesMux registers every module's routes exactly as cmd/api newServer
// does (same AddRoutes), against zero-value applications.
func allRoutesMux(verifier authn.Verifier, validator authn.StampValidator) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	identityports.AddRoutes(mux, log, identityapp.Application{}, verifier, validator)
	platformports.AddRoutes(mux, log, platformapp.Application{}, verifier, validator)
	inventoryports.AddRoutes(mux, log, inventoryapp.Application{}, verifier, validator)
	crmports.AddRoutes(mux, log, crmapp.Application{}, verifier, validator)
	tasksports.AddRoutes(mux, log, tasksapp.Application{}, verifier, validator)
	dispatchports.AddRoutes(mux, log, dispatchapp.Application{}, verifier, validator)
	ordersports.AddRoutes(mux, log, ordersapp.Application{}, verifier, validator)
	notificationsports.AddRoutes(mux, log, notificationsapp.Application{}, verifier, validator)
	return httpmw.Recover(log)(mux)
}

// serveRoute sends one request for a scanned route (wildcards filled with a
// UUID) with the bearer "tok" and returns the response.
func serveRoute(t *testing.T, h http.Handler, reg routeReg) *httptest.ResponseRecorder {
	t.Helper()
	path := scopeWildcardRE.ReplaceAllString(reg.Path, "0190a000-0000-7000-8000-000000000001")
	req := httptest.NewRequestWithContext(t.Context(), reg.Method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// apiRoutes returns every scanned /api/v1 route, failing the test on a scanner
// offence or an implausibly small scan.
func apiRoutes(t *testing.T) []routeReg {
	t.Helper()
	routes, offences := scanAllRoutes(t)
	failOnOffences(t, offences)
	var out []routeReg
	for _, reg := range routes {
		if strings.HasPrefix(reg.Path, "/api/v1/") {
			out = append(out, reg)
		}
	}
	if len(out) < 50 {
		t.Fatalf("scanner found only %d /api/v1 routes: the scan is broken", len(out))
	}
	return out
}

func tenantClaims() *jwt.Claims {
	c := &jwt.Claims{
		TenantID: "0190a000-0000-7000-8000-000000000001", TenantSlug: "acme",
		MembershipID: "0190a000-0000-7000-8000-0000000000aa", SecurityStamp: "stamp",
	}
	c.Subject = "0190a000-0000-7000-8000-0000000000ff"
	return c
}

func isCode(rec *httptest.ResponseRecorder, status int, code string) bool {
	return rec.Code == status && strings.Contains(rec.Body.String(), `"`+code+`"`)
}

// TestArch_MustChangePassword_BlocksEveryRouteButThePinnedAllowlist (plan D8):
// for EVERY registered /api/v1 route, a token carrying must_change_password
//   - is answered 403 password_change_required when the route is authenticated
//     and not on authn.PendingPasswordChangeAllowlist;
//   - reaches the handler on the allowlist (change-password, own capabilities);
//   - is never blocked on the pinned anonymous routes (they do not read a token).
//
// It also pins the anonymous set, asserts every allowlist entry is a registered
// authenticated route, and forbids a route registered with authn.RequireAuth
// alone (it would skip the stamp check, and with it the forced-change check).
//
// Scope: production — every route registered by every module's ports and cmd/api.
//
// arch-test:no-negative-fixture (RED proof: register any route with authn.RequireAuth, or add an entry to the allowlist → this test fails)
func TestArch_MustChangePassword_BlocksEveryRouteButThePinnedAllowlist(t *testing.T) {
	t.Parallel()
	flagged := tenantClaims()
	flagged.MustChangePassword = true
	h := allRoutesMux(tokenFake{flagged}, liveValidator{})

	allow := authn.PendingPasswordChangeAllowlist()
	var anonymous []string
	registered := map[string]bool{}
	for _, reg := range apiRoutes(t) {
		registered[reg.Pattern] = true
		if reg.has(tagBareAuth) {
			t.Errorf("%s:%d %s is registered with authn.RequireAuth alone: it skips the stamp check and so the forced-password-change check; use RequireFreshStamp or a permission gate", reg.File, reg.Line, reg.Pattern)
		}
		rec := serveRoute(t, h, reg)
		blocked := isCode(rec, http.StatusForbidden, "password_change_required")
		switch {
		case reg.firstGate() < 0:
			anonymous = append(anonymous, reg.Pattern)
			if blocked {
				t.Errorf("%s is anonymous but answered password_change_required", reg.Pattern)
			}
		case slices.Contains(allow, reg.Pattern):
			if blocked {
				t.Errorf("%s is on the allowlist but was blocked: %d %s", reg.Pattern, rec.Code, rec.Body.String())
			}
		case !blocked:
			t.Errorf("%s:%d %s: a must_change_password token got %d %s; want 403 password_change_required (every authenticated route not on the allowlist is blocked)", reg.File, reg.Line, reg.Pattern, rec.Code, rec.Body.String())
		}
	}
	slices.Sort(anonymous)
	if !slices.Equal(anonymous, anonymousAPIRoutes) {
		t.Errorf("anonymous /api/v1 routes changed.\n got:  %v\n want: %v\nA route that does not read a token escapes the forced change; adding one is a reviewed decision (anonymousAPIRoutes).", anonymous, anonymousAPIRoutes)
	}
	for _, entry := range allow {
		if !registered[entry] {
			t.Errorf("allowlist entry %q is not a registered route: remove it", entry)
		}
		if slices.Contains(anonymousAPIRoutes, entry) {
			t.Errorf("allowlist entry %q is anonymous: it needs no entry", entry)
		}
	}
}
