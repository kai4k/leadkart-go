package authn_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// serveOnMux serves one request through a real ServeMux, so the middleware sees
// the matched route pattern exactly as in production (r.Pattern).
func serveOnMux(t *testing.T, pattern string, claims *jwt.Claims, validator authn.StampValidator) (*httptest.ResponseRecorder, *sentinel) {
	t.Helper()
	return serveOnMuxWith(t, pattern, claims, validator, activeTenant)
}

// activeTenant is the tenant-status lookup of a company that is active.
func activeTenant(context.Context, string) (authn.TenantScopeState, error) {
	return authn.TenantScopeActive, nil
}

// serveOnMuxWith serves the route behind the same wrapper production puts around the
// mux (TenantScope, which carries the tenant-status lookup); a nil lookup serves it bare.
func serveOnMuxWith(t *testing.T, pattern string, claims *jwt.Claims, validator authn.StampValidator, lookup authn.TenantLookupFunc) (*httptest.ResponseRecorder, *sentinel) {
	t.Helper()
	s := &sentinel{}
	mux := http.NewServeMux()
	mux.Handle(pattern, authn.RequireFreshStamp(&fakeVerifier{wantToken: "tok", claims: claims}, validator)(s.handler()))
	var h http.Handler = mux
	if lookup != nil {
		h = authn.TenantScope(lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))(mux)
	}
	method, path, _ := strings.Cut(pattern, " ")
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, s
}

func flaggedClaims() *jwt.Claims {
	c := freshClaims(stampFresh)
	c.MustChangePassword = true
	return c
}

func errCodeOf(rec *httptest.ResponseRecorder) string {
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestRequireFreshStamp_MustChangePassword_BlocksEverythingButTheAllowlist(t *testing.T) {
	t.Parallel()
	validator := &fakeStampValidator{fresh: true}

	for _, pattern := range []string{
		"GET /api/v1/users",
		"GET /api/v1/crm/leads",
		"POST /api/v1/users/{userId}/roles",
		"POST /api/v1/permission-requests",
		"GET /api/v1/auth/sessions",
		"POST /api/v1/auth/request-email-change",
		"DELETE /api/v1/auth/sessions",
		"GET /api/v1/platform/stats",
	} {
		rec, s := serveOnMux(t, pattern, flaggedClaims(), validator)
		body := errCodeOf(rec)
		if rec.Code != http.StatusForbidden || !strings.Contains(body, `"password_change_required"`) || s.called {
			t.Errorf("%s: status %d called %v body %s; want 403 password_change_required and no next", pattern, rec.Code, s.called, body)
		}
		if rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s: a 403 must not carry WWW-Authenticate (the BFF reads 401+header as 'refresh')", pattern)
		}
	}

	for _, pattern := range authn.PendingPasswordChangeAllowlist() {
		rec, s := serveOnMux(t, pattern, flaggedClaims(), validator)
		if rec.Code != http.StatusOK || !s.called {
			t.Errorf("%s (allowlisted): status %d called %v; want 200", pattern, rec.Code, s.called)
		}
	}
}

func TestRequireFreshStamp_MustChangePassword_AllowlistIsExactlyPinned(t *testing.T) {
	t.Parallel()
	got := authn.PendingPasswordChangeAllowlist()
	want := []string{
		"GET /api/v1/auth/me/capabilities",
		"POST /api/v1/auth/change-password",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("allowlist = %v, want exactly %v", got, want)
	}
}

func TestRequireFreshStamp_MustChangePassword_NoRoutePatternIsBlocked(t *testing.T) {
	t.Parallel()
	// Called without a mux there is no route pattern: it cannot be on the
	// allowlist, so a flagged token is blocked (a new route is never exempt by default).
	s := &sentinel{}
	rec := httptest.NewRecorder()
	authn.RequireFreshStamp(&fakeVerifier{claims: flaggedClaims()}, &fakeStampValidator{fresh: true})(s.handler()).
		ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden || s.called {
		t.Fatalf("status %d called %v; want 403", rec.Code, s.called)
	}
}

func TestRequireFreshStamp_TokenWithoutTheFlagIsNotBlocked(t *testing.T) {
	t.Parallel()
	rec, s := serveOnMux(t, "GET /api/v1/users", freshClaims(stampFresh), &fakeStampValidator{fresh: true})
	if rec.Code != http.StatusOK || !s.called {
		t.Fatalf("un-flagged token (also: every token minted before the claim): status %d", rec.Code)
	}
}

func TestRequireFreshStamp_MustChangePassword_StaleStampStillWinsFirst(t *testing.T) {
	t.Parallel()
	rec, _ := serveOnMux(t, "GET /api/v1/users", flaggedClaims(), &fakeStampValidator{fresh: false})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d; a stale flagged token is 401 stale_token, not 403", rec.Code)
	}
}

func TestRequireFreshStamp_ImpersonationToken_NeverCarriesOrIsBlockedByTheFlag(t *testing.T) {
	t.Parallel()
	// The flag belongs to the person behind the token (the operator); a token
	// minted without it (impersonation mint never sets it) is not blocked.
	v := &sessionStampValidator{fakeStampValidator: fakeStampValidator{fresh: true}, live: true}
	rec, s := serveOnMux(t, "GET /api/v1/users", impersonationClaims(stampPersonID, stampPersonID, "sess-1"), v)
	if rec.Code != http.StatusOK || !s.called {
		t.Fatalf("status %d", rec.Code)
	}
}
