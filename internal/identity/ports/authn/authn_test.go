package authn_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// fakeVerifier is a minimal authn.Verifier for middleware tests.
// Production wires *jwt.Issuer; tests substitute this so they don't
// need to mint real HMAC-signed tokens.
type fakeVerifier struct {
	wantToken string
	claims    *jwt.Claims
	err       error
}

func (f *fakeVerifier) Verify(token string) (*jwt.Claims, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.wantToken != "" && token != f.wantToken {
		return nil, errors.New("fake: unexpected token")
	}
	return f.claims, nil
}

// alwaysFresh satisfies [authn.StampValidator] by reporting every
// stamp as fresh. Used by the perm/tenant-context/platform tests
// here that focus on authorization branches; freshness behaviour is
// covered exhaustively in security_stamp_test.go.
type alwaysFresh struct{}

func (alwaysFresh) IsFresh(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

// withFreshness fills in Subject + SecurityStamp on a Claims literal
// when the test focuses on a NON-freshness branch (permission /
// tenant-context / platform). RequireFreshStamp guards against empty
// Subject or SecurityStamp before consulting the validator (defense-
// in-depth against claim-stripping); this helper supplies non-empty
// placeholders so those guards don't fire and the test-under-attention
// reaches its assertion.
func withFreshness(c *jwt.Claims) *jwt.Claims {
	if c.Subject == "" {
		c.Subject = "01999999-aaaa-7000-8000-aaaaaaaaaaaa"
	}
	if c.SecurityStamp == "" {
		c.SecurityStamp = "00000000-0000-7000-8000-000000000001"
	}
	return c
}

// next is the protected handler — records that it was reached and lets
// tests inspect the claims attached to ctx via authn.ClaimsFromContext
// AND the tenant ID bound to ctx via tenancy.FromContext.
type sentinel struct {
	called   bool
	claims   *jwt.Claims
	tenantID tenancy.ID
}

func (s *sentinel) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.claims, _ = authn.ClaimsFromContext(r.Context())
		s.tenantID, _ = tenancy.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func newRequest(t *testing.T, authHeader string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func TestRequireAuth_MissingHeader_Returns401(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{claims: &jwt.Claims{TenantID: "tenant-test"}}
	s := &sentinel{}
	mw := authn.RequireAuth(v)(s.handler())

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next handler ran despite missing bearer")
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate: got %q want Bearer challenge", got)
	}
}

func TestRequireAuth_MalformedBearer_Returns401(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, header string
	}{
		{"raw token (no scheme)", "abcdef"},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"empty token after scheme", "Bearer "},
		{"Bearer-only", "Bearer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{}
			s := &sentinel{}
			mw := authn.RequireAuth(v)(s.handler())
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, newRequest(t, tc.header))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: got %d want 401", tc.name, rec.Code)
			}
			if s.called {
				t.Fatalf("%s: next ran", tc.name)
			}
		})
	}
}

func TestRequireAuth_VerifierFails_Returns401(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{err: jwt.ErrInvalidToken}
	s := &sentinel{}
	mw := authn.RequireAuth(v)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer fake.token.here"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite verify failure")
	}
}

func TestRequireAuth_ValidToken_PopulatesClaimsAndCallsNext(t *testing.T) {
	t.Parallel()
	want := &jwt.Claims{
		TenantID:    "tenant-1",
		IsSuperUser: false,
		Permissions: []string{permission.IdentityPermissions.Roles.View},
	}
	v := &fakeVerifier{wantToken: "good.token", claims: want}
	s := &sentinel{}
	mw := authn.RequireAuth(v)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer good.token"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("next did not run")
	}
	if s.claims != want {
		t.Fatalf("claims on ctx: got %+v want %+v", s.claims, want)
	}
}

func TestRequireAuth_BindsTenantContextFromClaim(t *testing.T) {
	// Per multi-tenancy.md: every authenticated request MUST have
	// tenant ctx bound so downstream repos under TxScopeTenant can
	// resolve `app.tenant_id` GUC. RequireAuth bridges JWT claim
	// tenant_id → tenancy.WithID(ctx) so handlers don't have to.
	t.Parallel()
	const wantTenant = "019dfe62-d263-7a20-b7de-08df2621c8eb"
	v := &fakeVerifier{
		wantToken: "tok",
		claims:    &jwt.Claims{TenantID: wantTenant},
	}
	s := &sentinel{}
	mw := authn.RequireAuth(v)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("next did not run")
	}
	if string(s.tenantID) != wantTenant {
		t.Fatalf("tenant ctx: got %q want %q", s.tenantID, wantTenant)
	}
}

func TestRequireAuth_EmptyTenantIDClaim_Returns401(t *testing.T) {
	// A token with empty tenant_id is a JWT-issuance bug — every
	// production-issued token populates tenant_id. Treat as
	// unauthenticated rather than letting the request reach a
	// handler with an unbound tenant ctx (would fail opaquely at
	// the repo layer when TxScopeTenant tried to set the GUC).
	t.Parallel()
	cases := []struct {
		name      string
		tenantID  string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{
				wantToken: "tok",
				claims:    &jwt.Claims{TenantID: tc.tenantID},
			}
			s := &sentinel{}
			mw := authn.RequireAuth(v)(s.handler())
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: got %d want 401", rec.Code)
			}
			if s.called {
				t.Fatal("next ran despite missing tenant_id claim")
			}
		})
	}
}

func TestRequireAuth_AcceptsCaseInsensitiveScheme(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{wantToken: "tok", claims: &jwt.Claims{TenantID: "tenant-test"}}
	s := &sentinel{}
	mw := authn.RequireAuth(v)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("lowercase bearer: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("next did not run on lowercase bearer scheme")
	}
}

// ----- RequirePermission ---------------------------------------------------

func TestRequirePermission_NoBearer_Returns401(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{}
	s := &sentinel{}
	mw := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Roles.View)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
}

func TestRequirePermission_ClaimsLackPermission_Returns403(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:    "tenant-test",
			Permissions: []string{permission.IdentityPermissions.Users.View}, // wrong perm
		}),
	}
	s := &sentinel{}
	mw := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Roles.View)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	if s.called {
		t.Fatal("next ran despite missing permission")
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "forbidden") {
		t.Fatalf("body: got %q want forbidden code", string(body))
	}
}

func TestRequirePermission_PermissionPresent_Returns200(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID: "tenant-test",
			Permissions: []string{
				permission.IdentityPermissions.Roles.View,
				permission.IdentityPermissions.Users.View,
			},
		}),
	}
	s := &sentinel{}
	mw := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Roles.View)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("next did not run")
	}
}

func TestRequirePermission_SuperUser_BypassesCheck(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:    "tenant-test",
			IsSuperUser: true,
			// Empty permissions on purpose: SuperUser short-circuits.
			Permissions: nil,
		}),
	}
	s := &sentinel{}
	mw := authn.RequirePermission(v, alwaysFresh{},
		permission.IdentityPermissions.Tenants.Delete)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("SuperUser bypass: got %d want 200", rec.Code)
	}
	if !s.called {
		t.Fatal("SuperUser bypass: next did not run")
	}
}

func TestRequirePermission_PanicsOnUnknownPermissionName(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unknown permission name (wiring bug)")
		}
	}()
	_ = authn.RequirePermission(v, alwaysFresh{}, "made.up.permission.x") // arch-test:ignore-err - test fixture setup
}

func TestRequireAuth_PanicsOnNilVerifier(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil verifier")
		}
	}()
	_ = authn.RequireAuth(nil) // arch-test:ignore-err - test fixture setup
}

// ----- RequireAnyPermission ------------------------------------------------

func TestRequireAnyPermission_OneOfManyPresent_Returns200(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:    "tenant-test",
			Permissions: []string{permission.IdentityPermissions.Roles.View},
		}),
	}
	s := &sentinel{}
	mw := authn.RequireAnyPermission(v, alwaysFresh{},
		permission.IdentityPermissions.Roles.View,
		permission.IdentityPermissions.Roles.Update,
	)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
}

func TestRequireAnyPermission_NonePresent_Returns403(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:    "tenant-test",
			Permissions: []string{permission.IdentityPermissions.Users.View},
		}),
	}
	s := &sentinel{}
	mw := authn.RequireAnyPermission(v, alwaysFresh{},
		permission.IdentityPermissions.Roles.View,
		permission.IdentityPermissions.Roles.Update,
	)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
}

func TestRequireAnyPermission_SuperUser_Bypass(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims:    withFreshness(&jwt.Claims{TenantID: "tenant-test", IsSuperUser: true}),
	}
	s := &sentinel{}
	mw := authn.RequireAnyPermission(v, alwaysFresh{},
		permission.IdentityPermissions.Roles.Delete,
	)(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("super-user bypass: got %d want 200", rec.Code)
	}
}

func TestRequireAnyPermission_PanicsOnEmptyList(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty permission list")
		}
	}()
	_ = authn.RequireAnyPermission(v, alwaysFresh{}) // arch-test:ignore-err - test fixture setup
}

// ----- RequirePlatform -----------------------------------------------------

func TestRequirePlatform_TokenIsPlatform_Returns200(t *testing.T) {
	t.Parallel()
	// Slug-anchor + IsPlatform flag together — defense-in-depth check
	// per migration 20260507000008.
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:   "tenant-test",
			TenantSlug: authn.PlatformTenantSlug,
			IsPlatform: true,
		}),
	}
	s := &sentinel{}
	mw := authn.RequirePlatform(v, alwaysFresh{})(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
}

func TestRequirePlatform_IsPlatformWithoutSlug_Returns403(t *testing.T) {
	// Spoofed/bug case: JWT carries is_platform=true but tenant_slug
	// is NOT 'platform'. Slug anchor catches it — defense-in-depth.
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims: withFreshness(&jwt.Claims{
			TenantID:   "tenant-test",
			TenantSlug: "some-other-slug",
			IsPlatform: true,
		}),
	}
	s := &sentinel{}
	mw := authn.RequirePlatform(v, alwaysFresh{})(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (slug anchor must reject IsPlatform without matching slug)", rec.Code)
	}
}

func TestRequirePlatform_TenantToken_Returns403(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{
		wantToken: "tok",
		claims:    withFreshness(&jwt.Claims{TenantID: "tenant-test", IsPlatform: false}),
	}
	s := &sentinel{}
	mw := authn.RequirePlatform(v, alwaysFresh{})(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	if s.called {
		t.Fatal("next ran for non-platform token")
	}
}

func TestRequirePlatform_NoBearer_Returns401(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{}
	s := &sentinel{}
	mw := authn.RequirePlatform(v, alwaysFresh{})(s.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, newRequest(t, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
}
