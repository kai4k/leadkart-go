package authn_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const (
	tenantA = "0190a000-0000-7000-8000-00000000000a"
	tenantB = "0190a000-0000-7000-8000-00000000000b"
	selfMem = "0190a000-0000-7000-8000-0000000000aa"
)

func TestSatisfies_DecisionTable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name      string
		claims    *jwt.Claims
		effective string
		perm      string
		want      bool
	}{
		{"explicit grant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, tenantA, p.Users.View, true},
		{"missing", &jwt.Claims{TenantID: tenantA}, tenantA, p.Users.View, false},
		{"super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantB, p.Users.Anonymise, true},
		{"tenant.admin own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Roles.Assign, true},
		{"tenant.admin other module", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Crm.Leads.ReadAll, true},
		{"tenant.admin other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, p.Roles.Assign, false},
		{"tenant.admin reserved", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Tenants.Suspend, false},
		{"platform equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, true},
		{"platform equivalent needs slug", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, Permissions: []string{p.Platform.UsersView}}, tenantB, p.Users.View, false},
		{"platform equivalent not for impersonation", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView}, Act: &jwt.ActClaim{Sub: "op"}}, tenantB, p.Users.View, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := authn.WithClaims(t.Context(), tc.claims)
			ctx = tenancy.WithID(ctx, tenancy.ID(tc.effective))
			if got := authn.Satisfies(ctx, tc.perm); got != tc.want {
				t.Fatalf("Satisfies = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSatisfies_NoClaims_False(t *testing.T) {
	t.Parallel()
	if authn.Satisfies(t.Context(), permission.IdentityPermissions.Users.View) {
		t.Fatal("Satisfies without claims must be false")
	}
}

func TestEffectivePermissions_TenantAdminExpandsAndSorts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	got := authn.EffectivePermissions(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
	if !slices.IsSorted(got) {
		t.Fatal("not sorted")
	}
	for _, want := range []string{p.Meta.TenantAdmin, p.Users.Create, p.Crm.Leads.Read} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if slices.Contains(got, p.Platform.TenantsManage) {
		t.Error("tenant.admin must not expand to platform.tenants.manage")
	}
}

func TestRequirePermission_TenantAdminPasses(t *testing.T) {
	t.Parallel()
	v := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{permission.IdentityPermissions.Meta.TenantAdmin}})}
	s := &sentinel{}
	h := authn.RequirePermission(v, alwaysFresh{}, permission.IdentityPermissions.Users.View)(s.handler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != http.StatusOK || !s.called {
		t.Fatalf("status %d called %v, want 200 true", rec.Code, s.called)
	}
}

func tenantReq(t *testing.T, tenantID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /t/{tenantId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/t/"+tenantID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequireTenantPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"owner own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, 200},
		{"member own tenant no perm", &jwt.Claims{TenantID: tenantA}, tenantA, 403},
		{"owner other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, 403},
		{"platform view equivalent", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.TenantsView}}, tenantB, 200},
		{"platform without perm", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true}, tenantB, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &fakeVerifier{claims: withFreshness(tc.claims)}
			s := &sentinel{}
			h := authn.RequireTenantPermission(v, alwaysFresh{}, "tenantId", p.Tenants.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tenantReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func userReq(t *testing.T, userID string) *http.Request {
	t.Helper()
	mux := http.NewServeMux()
	var captured *http.Request
	mux.HandleFunc("GET /u/{userId}", func(_ http.ResponseWriter, r *http.Request) { captured = r })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/u/"+userID, nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	captured.Header.Set("Authorization", "Bearer tok")
	return captured
}

func TestRequirePermissionOrSelf(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	member := func() *jwt.Claims { return withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem}) }
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self", member(), selfMem, 200},
		{"other without perm", member(), "0190a000-0000-7000-8000-0000000000bb", 403},
		{"other with perm", withFreshness(&jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Permissions: []string{p.Users.View}}), "0190a000-0000-7000-8000-0000000000bb", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequirePermissionOrSelf(&fakeVerifier{claims: tc.claims}, alwaysFresh{}, "userId", p.Users.View)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestDenySelfTarget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		path   string
		want   int
	}{
		{"self denied", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, selfMem, 403},
		{"other allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem}, "0190a000-0000-7000-8000-0000000000bb", 200},
		{"super user self allowed", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, IsSuperUser: true}, selfMem, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequireFreshStamp(&fakeVerifier{claims: withFreshness(tc.claims)}, alwaysFresh{})(authn.DenySelfTarget("userId")(s.handler()))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, userReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
			if tc.want == 403 && !strings.Contains(rec.Body.String(), "self_modification_forbidden") {
				t.Fatalf("body %s", rec.Body.String())
			}
		})
	}
}
