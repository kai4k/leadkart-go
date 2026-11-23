package authn_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/actclaim"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const gateOperatorID = "0190a000-0000-7000-8000-0000000000a1"

func platformMember(perms ...string) *jwt.Claims {
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: perms}
	c.Subject = gateOperatorID
	return c
}

// gateProbe runs one request through RequirePlatformGate(perms...) and reports
// the status plus the act claim the protected handler saw.
func gateProbe(t *testing.T, c *jwt.Claims, perms ...string) (int, actclaim.Claim, bool) {
	t.Helper()
	var got actclaim.Claim
	var has bool
	h := authn.RequirePlatformGate(&fakeVerifier{claims: withFreshness(c)}, alwaysFresh{})(perms...)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, has = actclaim.FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, got, has
}

func TestRequirePlatformGate_ZeroPermissionPlatformMember_403(t *testing.T) {
	t.Parallel()
	if code, _, _ := gateProbe(t, platformMember(), permission.IdentityPermissions.Tenants.Suspend); code != http.StatusForbidden {
		t.Fatalf("zero-permission platform member: %d, want 403", code)
	}
}

// Each named permission is required: holding all the others is not enough.
func TestRequirePlatformGate_EveryNamedPermissionIsRequired(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	tests := []struct {
		name  string
		need  []string
		holds []string
		want  int
	}{
		{"single, held", []string{p.Tenants.Suspend}, []string{p.Tenants.Suspend}, http.StatusOK},
		{"single, other held", []string{p.Tenants.Suspend}, []string{p.Tenants.Activate}, http.StatusForbidden},
		{"two, both held", []string{p.Platform.TenantsView, p.Platform.UsersView}, []string{p.Platform.TenantsView, p.Platform.UsersView}, http.StatusOK},
		{"two, first only", []string{p.Platform.TenantsView, p.Platform.UsersView}, []string{p.Platform.TenantsView}, http.StatusForbidden},
		{"two, second only", []string{p.Platform.TenantsView, p.Platform.UsersView}, []string{p.Platform.UsersView}, http.StatusForbidden},
		{"two, none", []string{p.Platform.TenantsView, p.Platform.UsersView}, nil, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if code, _, _ := gateProbe(t, platformMember(tt.holds...), tt.need...); code != tt.want {
				t.Fatalf("status %d, want %d", code, tt.want)
			}
		})
	}
}

func TestRequirePlatformGate_SuperUserPasses(t *testing.T) {
	t.Parallel()
	c := platformMember()
	c.IsSuperUser = true
	if code, _, _ := gateProbe(t, c, permission.IdentityPermissions.Tenants.Suspend); code != http.StatusOK {
		t.Fatalf("super user: %d, want 200", code)
	}
}

// tenant.admin of an ordinary tenant is not a platform actor, whatever else
// its claims say.
func TestRequirePlatformGate_OrdinaryTenantAdmin_403(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	c := &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", Permissions: []string{p.Meta.TenantAdmin, p.Tenants.Suspend}}
	c.Subject = gateOperatorID
	if code, _, _ := gateProbe(t, c, p.Tenants.Suspend); code != http.StatusForbidden {
		t.Fatalf("ordinary tenant.admin: %d, want 403", code)
	}
	c.IsPlatform = true // is_platform without the platform slug anchor
	if code, _, _ := gateProbe(t, c, p.Tenants.Suspend); code != http.StatusForbidden {
		t.Fatalf("is_platform without slug anchor: %d, want 403", code)
	}
}

func TestRequirePlatformGate_ImpersonationToken_403(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	c := platformMember(p.Tenants.Suspend)
	c.Act = &jwt.ActClaim{Sub: gateOperatorID, SessionID: "0190a000-0000-7000-8000-0000000000a2", Reason: "ticket"}
	if code, _, _ := gateProbe(t, c, p.Tenants.Suspend); code != http.StatusForbidden {
		t.Fatalf("impersonation token: %d, want 403", code)
	}
}

func TestRequirePlatformGate_NoPermissionNames_PanicsAtWiring(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("RequirePlatformGate()() with no permission names must panic at wiring time")
		}
	}()
	authn.RequirePlatformGate(&fakeVerifier{}, alwaysFresh{})()
}

// F1: the gate attributes the caller when nothing else has.
func TestRequirePlatformGate_SetsOperatorActClaim(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	code, got, has := gateProbe(t, platformMember(p.Tenants.Suspend), p.Tenants.Suspend)
	if code != http.StatusOK || !has {
		t.Fatalf("status %d, claim present %v; want 200 with an act claim", code, has)
	}
	if got.OperatorID != gateOperatorID || got.Reason != authn.ScopeActReasonPlatform {
		t.Fatalf("claim = %+v, want operator %s reason %q", got, gateOperatorID, authn.ScopeActReasonPlatform)
	}
}

// An act claim a scope or path gate already set is not overwritten.
func TestRequirePlatformGate_PreservesExistingActClaim(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	existing := actclaim.Claim{OperatorID: gateOperatorID, Reason: authn.ScopeActReasonPath}
	var got actclaim.Claim
	gate := authn.RequirePlatformGate(&fakeVerifier{claims: withFreshness(platformMember(p.Tenants.Suspend))}, alwaysFresh{})(p.Tenants.Suspend)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, _ = actclaim.FromContext(r.Context())
		})).ServeHTTP(w, r.WithContext(actclaim.WithContext(r.Context(), existing)))
	})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer tok")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != existing {
		t.Fatalf("claim = %+v, want the existing %+v kept", got, existing)
	}
}
