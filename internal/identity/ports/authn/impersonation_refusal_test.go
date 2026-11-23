package authn_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- impersonation refusal list --------------------------------------------

func TestRequireFreshStamp_ImpersonationToken_RefusedOnEveryPinnedRoute(t *testing.T) {
	t.Parallel()
	for _, pattern := range append(authn.ImpersonationRefusals(),
		"GET /api/v1/platform/anything-new", // the whole platform plane
		"POST /api/v1/platform/impersonation/sessions",
	) {
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		rec, s := serveOnMux(t, pattern, impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
		body := errCodeOf(rec)
		if rec.Code != http.StatusForbidden || !strings.Contains(body, `"impersonation_not_allowed"`) || s.called {
			t.Errorf("%s: status %d called %v body %s; want 403 impersonation_not_allowed", pattern, rec.Code, s.called, body)
		}
	}
}

func TestRequireFreshStamp_ImpersonationToken_AllowedOnOrdinaryAndCapabilitiesRoutes(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"GET /api/v1/users",
		"POST /api/v1/users",
		"GET /api/v1/crm/leads",
		"GET /api/v1/auth/me/capabilities",
		"GET /api/v1/permission-requests",
	} {
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		rec, s := serveOnMux(t, pattern, impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
		if rec.Code != http.StatusOK || !s.called {
			t.Errorf("%s: status %d called %v; want 200", pattern, rec.Code, s.called)
		}
	}
}

func TestRequireFreshStamp_OrdinaryTokenIsNotAffectedByTheRefusalList(t *testing.T) {
	t.Parallel()
	for _, pattern := range authn.ImpersonationRefusals() {
		rec, s := serveOnMux(t, pattern, freshClaims(stampFresh), &fakeStampValidator{fresh: true})
		if rec.Code != http.StatusOK || !s.called {
			t.Errorf("%s: an ordinary token got %d", pattern, rec.Code)
		}
	}
}

func TestRequireFreshStamp_ImpersonationRefusal_DeadSessionIs401NotRefusal(t *testing.T) {
	t.Parallel()
	v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: false}
	rec, _ := serveOnMux(t, "POST /api/v1/auth/change-password", impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d; an ended session is 401 stale_token before any refusal", rec.Code)
	}
}

// ----- fail closed (fix round item 3) ----------------------------------------

// A request that reaches the middleware without a matched route pattern (a router
// or wrapper that lost it) cannot be checked against the list, so an
// impersonation token is refused rather than waved through.
func TestRequireFreshStamp_ImpersonationToken_NoRoutePatternIsRefused(t *testing.T) {
	t.Parallel()
	v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
	rec, s := serveWith(t, impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
	body := errCodeOf(rec)
	if rec.Code != http.StatusForbidden || !strings.Contains(body, `"impersonation_not_allowed"`) || s.called {
		t.Fatalf("no pattern: status %d called %v body %s; want 403 impersonation_not_allowed", rec.Code, s.called, body)
	}
}

func TestImpersonationRefused_FailsClosedOnShapesItCannotParse(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                            true, // no pattern at all
		"/api/v1/platform/stats":      true, // method-less pattern: a path
		"/api/v1/platform/{anything}": true,
		"GET /api/v1/platform/stats":  true,
		"/api/v1/users":               false, // method-less, outside the plane, not listed
		"GET /api/v1/users":           false,
		"GET /api/v1/auth/sessions":   true,  // pinned list
		"/api/v1/platform":            false, // the prefix needs its trailing slash
		"POST /api/v1/platform/marketplace/leads/{id}/purchase": true,
	}
	for pattern, want := range cases {
		if got := authn.ImpersonationRefused(pattern); got != want {
			t.Errorf("ImpersonationRefused(%q) = %v; want %v", pattern, got, want)
		}
	}
}

// ----- the two tenant-plane reads under /platform/ (fix round item 5) ---------

func TestImpersonationRefused_TheTwoTenantPlaneReadsStayOpen(t *testing.T) {
	t.Parallel()
	open := []string{
		"GET /api/v1/platform/marketplace/leads",
		"GET /api/v1/platform/lead-credits/balance",
	}
	if got := authn.ImpersonationOpenPlatformReads(); !slicesEqual(got, open) {
		t.Fatalf("ImpersonationOpenPlatformReads = %v; want exactly %v", got, open)
	}
	for _, pattern := range open {
		if authn.ImpersonationRefused(pattern) {
			t.Errorf("%s must be open to an impersonation session (a tenant-plane read gated by tenant permissions)", pattern)
		}
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		rec, s := serveOnMux(t, pattern, impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
		if rec.Code != http.StatusOK || !s.called {
			t.Errorf("%s: status %d called %v; want 200", pattern, rec.Code, s.called)
		}
	}
	// Spending and every other platform route stay refused; the method matters.
	for _, pattern := range []string{
		"POST /api/v1/platform/marketplace/leads/{id}/purchase",
		"POST /api/v1/platform/lead-credits/topup",
		"POST /api/v1/platform/marketplace/leads", // same path, other method
		"GET /api/v1/platform/stats",
	} {
		if !authn.ImpersonationRefused(pattern) {
			t.Errorf("%s must stay refused", pattern)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ----- a company that is no longer active (review G2c N1) --------------------

func suspendedTenant(context.Context, string) (authn.TenantScopeState, error) {
	return authn.TenantScopeInactive, nil
}

// A session opened before the company was suspended writes nothing: every non-read
// method answers 409 tenant_not_active. Reads stay open.
func TestRequireFreshStamp_ImpersonationToken_WritesRefusedOnASuspendedCompany(t *testing.T) {
	t.Parallel()
	claims := func() *jwt.Claims { return impersonationClaims(stampPersonID, stampPersonID, "sess-1") }
	for _, pattern := range []string{"POST /api/v1/users", "PUT /api/v1/users", "PATCH /api/v1/users", "DELETE /api/v1/users"} {
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		rec, s := serveOnMuxWith(t, pattern, claims(), v, suspendedTenant)
		if body := errCodeOf(rec); rec.Code != http.StatusConflict || !strings.Contains(body, `"tenant_not_active"`) || s.called {
			t.Errorf("%s: status %d called %v body %s; want 409 tenant_not_active", pattern, rec.Code, s.called, body)
		}
	}
	for _, pattern := range []string{"GET /api/v1/users", "HEAD /api/v1/users"} {
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		if rec, s := serveOnMuxWith(t, pattern, claims(), v, suspendedTenant); rec.Code != http.StatusOK || !s.called {
			t.Errorf("%s: status %d called %v; want 200 (a read is open)", pattern, rec.Code, s.called)
		}
	}
}

// An ordinary token is not asked: its company is judged at login and refresh.
func TestRequireFreshStamp_OrdinaryTokenWritesAreNotAskedAboutTheCompany(t *testing.T) {
	t.Parallel()
	asked := false
	lookup := func(context.Context, string) (authn.TenantScopeState, error) {
		asked = true
		return authn.TenantScopeInactive, nil
	}
	rec, s := serveOnMuxWith(t, "POST /api/v1/users", freshClaims(stampFresh), &fakeStampValidator{fresh: true}, lookup)
	if rec.Code != http.StatusOK || !s.called || asked {
		t.Fatalf("ordinary write: status %d called %v asked %v; want 200 and no lookup", rec.Code, s.called, asked)
	}
}

// With no way to tell (no lookup on the request, or the status unreadable) a session's
// write is refused, not waved through.
func TestRequireFreshStamp_ImpersonationToken_WriteFailsClosedWhenTheCompanyCannotBeRead(t *testing.T) {
	t.Parallel()
	broken := func(context.Context, string) (authn.TenantScopeState, error) {
		return authn.TenantScopeUnknown, context.DeadlineExceeded
	}
	for name, lookup := range map[string]authn.TenantLookupFunc{"no lookup": nil, "unreadable": broken} {
		v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
		rec, s := serveOnMuxWith(t, "POST /api/v1/users", impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v, lookup)
		if rec.Code != http.StatusInternalServerError || s.called {
			t.Errorf("%s: status %d called %v; want 500 and no write", name, rec.Code, s.called)
		}
	}
}
