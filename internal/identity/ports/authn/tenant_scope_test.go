package authn_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

type fakeTenants map[string]bool

func (f fakeTenants) TenantExists(_ context.Context, id string) (bool, error) { return f[id], nil }

func scopeChain(c *jwt.Claims, s *sentinel) http.Handler {
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return authn.TenantScope(v, fakeTenants{tenantB: true}.TenantExists, log)(authn.RequireFreshStamp(v, alwaysFresh{})(s.handler()))
}

func scopeReq(t *testing.T, header string) *http.Request {
	t.Helper()
	r := newRequest(t, "Bearer tok")
	if header != "" {
		r.Header.Set(authn.TenantScopeHeader, header)
	}
	return r
}

func TestTenantScope(t *testing.T) {
	t.Parallel()
	op := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}
	}
	member := func() *jwt.Claims { return &jwt.Claims{TenantID: tenantA, TenantSlug: "acme"} }
	imp := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", Act: &jwt.ActClaim{Sub: "op"}}
	}
	impOperator := func() *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Act: &jwt.ActClaim{Sub: "op"}}
	}
	nonSlug := func() *jwt.Claims { return &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true} }
	cases := []struct {
		name       string
		claims     *jwt.Claims
		header     string
		wantStatus int
		wantTenant string
	}{
		{"operator switches", op(), tenantB, 200, tenantB},
		{"operator switches, upper-case spelling", op(), "0190A000-0000-7000-8000-00000000000B", 200, tenantB},
		{"operator no header", op(), "", 200, tenantA},
		{"operator malformed", op(), "nope", 400, ""},
		{"operator unknown tenant", op(), "0190a000-0000-7000-8000-0000000000ee", 404, ""},
		{"member header ignored", member(), tenantB, 200, tenantA},
		{"member malformed header ignored", member(), "nope", 200, tenantA},
		{"impersonation header ignored", imp(), tenantB, 200, tenantA},
		{"impersonation of platform operator header ignored", impOperator(), tenantB, 200, tenantA},
		{"is_platform without slug anchor ignored", nonSlug(), tenantB, 200, tenantA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			rec := httptest.NewRecorder()
			scopeChain(tc.claims, s).ServeHTTP(rec, scopeReq(t, tc.header))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d want %d body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantTenant != "" && string(s.tenantID) != tc.wantTenant {
				t.Fatalf("tenant %q want %q", s.tenantID, tc.wantTenant)
			}
		})
	}
}

func TestTenantScope_ErrorCodes(t *testing.T) {
	t.Parallel()
	op := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}
	for header, want := range map[string]string{
		"nope":                                 "invalid_tenant_scope",
		"0190a000-0000-7000-8000-0000000000ee": "tenant_scope_not_found",
	} {
		rec := httptest.NewRecorder()
		scopeChain(op, &sentinel{}).ServeHTTP(rec, scopeReq(t, header))
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("header %q: body %s want code %s", header, rec.Body.String(), want)
		}
	}
}

func TestTenantScope_SatisfiesHonoursEffectiveTenant(t *testing.T) {
	t.Parallel()
	// Operator holding tenant.admin in the PLATFORM tenant gains nothing in tenant B (D1 own-tenant rule).
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"tenant.admin"}}
	var allowed bool
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := authn.TenantScope(v, fakeTenants{tenantB: true}.TenantExists, log)(authn.RequireFreshStamp(v, alwaysFresh{})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			allowed = authn.Satisfies(r.Context(), "identity.users.view")
		})))
	h.ServeHTTP(httptest.NewRecorder(), scopeReq(t, tenantB))
	if allowed {
		t.Fatal("tenant.admin must not imply permissions in a header-scoped foreign tenant")
	}
}

func TestTenantScope_PlatformEquivalentHoldsInScopedTenant(t *testing.T) {
	t.Parallel()
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{"platform.users.view"}}
	var allowed bool
	v := &fakeVerifier{claims: withFreshness(c)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := authn.TenantScope(v, fakeTenants{tenantB: true}.TenantExists, log)(authn.RequireFreshStamp(v, alwaysFresh{})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			allowed = authn.Satisfies(r.Context(), "identity.users.view")
		})))
	h.ServeHTTP(httptest.NewRecorder(), scopeReq(t, tenantB))
	if !allowed {
		t.Fatal("platform.users.view must satisfy identity.users.view in the scoped tenant")
	}
}

func TestTenantScope_AnonymousAndBadTokenPassThrough(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{err: context.Canceled}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var called bool
	h := authn.TenantScope(v, fakeTenants{}.TenantExists, log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))
	for _, auth := range []string{"", "Bearer bad"} {
		called = false
		r := newRequest(t, auth)
		r.Header.Set(authn.TenantScopeHeader, "nope")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if !called || rec.Code != http.StatusTeapot {
			t.Fatalf("auth %q: must pass through untouched, got %d", auth, rec.Code)
		}
	}
}

func TestActorMembershipID(t *testing.T) {
	t.Parallel()
	const m = "0190a000-0000-7000-8000-0000000000aa"
	c := &jwt.Claims{TenantID: tenantA, MembershipID: m}
	own := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(tenantA))
	foreign := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(tenantB))
	if got := authn.ActorMembershipID(own); got != m {
		t.Fatalf("own tenant: %q", got)
	}
	if got := authn.ActorMembershipID(foreign); got != "" {
		t.Fatalf("foreign scoped tenant must not stamp the operator membership: %q", got)
	}
	if got := authn.ActorMembershipID(t.Context()); got != "" {
		t.Fatalf("no claims: %q", got)
	}
}

func TestEffectiveTenantID(t *testing.T) {
	t.Parallel()
	c := &jwt.Claims{TenantID: tenantA}
	if id, ok := authn.EffectiveTenantID(tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(tenantB))); !ok || id != tenantB {
		t.Fatalf("bound tenant wins: %q %v", id, ok)
	}
	if id, ok := authn.EffectiveTenantID(authn.WithClaims(t.Context(), c)); !ok || id != tenantA {
		t.Fatalf("falls back to claims tenant: %q %v", id, ok)
	}
	if _, ok := authn.EffectiveTenantID(t.Context()); ok {
		t.Fatal("no tenant, no claims -> not ok")
	}
}
