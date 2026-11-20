package authn_test

import (
	"net/http"
	"strings"
	"testing"

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
