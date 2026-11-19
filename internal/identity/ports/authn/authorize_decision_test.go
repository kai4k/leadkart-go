package authn_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Decision-table rows for the single authorisation decision (ADR 0036/0068).
// Rule order as implemented in satisfiesIn:
//  1. no claims or a name outside the catalogue -> deny
//  2. super user -> allow
//  3. explicit grant: reserved name (other than tenant.admin) -> slug-anchored
//     platform operator only; any other name -> only in the caller's own tenant
//  4. tenant.admin in the caller's own tenant implies every non-reserved name
//  5. slug-anchored platform operator + platform.* equivalent -> any tenant
func TestSatisfies_DecisionTable_Hardening(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	operator := func(perms ...string) *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: perms}
	}
	impersonation := &jwt.Claims{TenantID: tenantB, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}
	cases := []struct {
		name      string
		claims    *jwt.Claims
		effective string
		perm      string
		want      bool
	}{
		// item 2: explicit grants are tenant-scoped
		{"explicit grant other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, tenantB, p.Users.View, false},
		{"explicit grant own tenant spelled uppercase", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, strings.ToUpper(tenantA), p.Users.View, true},
		{"operator tenant.admin other tenant", operator(p.Meta.TenantAdmin), tenantB, p.Users.View, false},
		{"operator tenant.admin own tenant", operator(p.Meta.TenantAdmin), tenantA, p.Users.View, true},
		{"explicit tenant.admin requirement other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantB, p.Meta.TenantAdmin, false},
		{"explicit tenant.admin requirement own tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}}, tenantA, p.Meta.TenantAdmin, true},
		{"operator platform.users.view gives users.view other tenant", operator(p.Platform.UsersView), tenantB, p.Users.View, true},
		// item 3: reserved names need a platform operator even when explicit
		{"tenant user explicit platform.tenants.manage", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Platform.TenantsManage}}, tenantA, p.Platform.TenantsManage, false},
		{"operator explicit platform.tenants.manage", operator(p.Platform.TenantsManage), tenantB, p.Platform.TenantsManage, true},
		{"tenant user explicit tenants.suspend", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Tenants.Suspend}}, tenantA, p.Tenants.Suspend, false},
		{"tenant user explicit users.anonymise", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.Anonymise}}, tenantA, p.Users.Anonymise, false},
		{"non-slug platform flag explicit platform perm", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, Permissions: []string{p.Platform.TenantsManage}}, tenantA, p.Platform.TenantsManage, false},
		// item 5: unknown / empty names
		{"unknown name super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantA, "not.a.permission", false},
		{"unknown name explicit", &jwt.Claims{TenantID: tenantA, Permissions: []string{"not.a.permission"}}, tenantA, "not.a.permission", false},
		{"empty name super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantA, "", false},
		{"empty name explicit", &jwt.Claims{TenantID: tenantA, Permissions: []string{""}}, tenantA, "", false},
		// item 6: impersonation token (tenant.admin, is_platform=false, act set)
		{"impersonation own target tenant", impersonation, tenantB, p.Users.Create, true},
		{"impersonation other tenant", impersonation, tenantA, p.Users.Create, false},
		{"impersonation reserved", impersonation, tenantB, p.Platform.TenantsManage, false},
		{"impersonation tenants.suspend", impersonation, tenantB, p.Tenants.Suspend, false},
		// item 6: platform equivalents are exact
		{"operator platform.tenants.manage is not tenants.suspend", operator(p.Platform.TenantsManage), tenantB, p.Tenants.Suspend, false},
		{"operator platform equivalent not for crm", operator(p.Platform.UsersManage, p.Platform.RolesManage), tenantB, p.Crm.Leads.Read, false},
		{"operator users.manage gives users.deactivate other tenant", operator(p.Platform.UsersManage), tenantB, p.Users.Deactivate, true},
		{"operator users.manage is not users.anonymise", operator(p.Platform.UsersManage), tenantB, p.Users.Anonymise, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := authn.WithClaims(t.Context(), tc.claims)
			ctx = tenancy.WithID(ctx, tenancy.ID(tc.effective))
			if got := authn.Satisfies(ctx, tc.perm); got != tc.want {
				t.Fatalf("Satisfies(%q in %q) = %v, want %v", tc.perm, tc.effective, got, tc.want)
			}
		})
	}
}

// Satisfies without a tenant on ctx falls back to the caller's own tenant
// (documented in its godoc): RequireAuth always binds one, so this only
// happens in non-HTTP callers and tests.
func TestSatisfies_NoTenantInContext_FallsBackToClaimsTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ctx := authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}})
	if !authn.Satisfies(ctx, p.Users.View) {
		t.Fatal("explicit grant must hold in the caller's own tenant when ctx carries none")
	}
	ctx = authn.WithClaims(t.Context(), &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})
	if !authn.Satisfies(ctx, p.Roles.Assign) {
		t.Fatal("tenant.admin must imply in the caller's own tenant when ctx carries none")
	}
}

func TestRequireTenantPermission_MoreCases(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name   string
		claims *jwt.Claims
		path   string
		perms  []string
		want   int
	}{
		{"super user other tenant", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, tenantB, []string{p.Tenants.View}, 200},
		{"impersonation token other tenant", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}, tenantB, []string{p.Tenants.View}, 403},
		{"impersonation token target tenant", &jwt.Claims{TenantID: tenantB, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}, tenantB, []string{p.Tenants.View}, 200},
		{"any-of second permission satisfied", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Tenants.Update}}, tenantA, []string{p.Tenants.View, p.Tenants.Update}, 200},
		{"unparseable path", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Tenants.View}}, "nope", []string{p.Tenants.View}, 400},
		{"any-of none satisfied", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.View}}, tenantA, []string{p.Tenants.View, p.Tenants.Update}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &sentinel{}
			h := authn.RequireTenantPermission(&fakeVerifier{claims: withFreshness(tc.claims)}, alwaysFresh{}, "tenantId", tc.perms...)(s.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tenantReq(t, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Item 4: the gate decides in the PATH tenant and re-binds ctx to it, so an
// inline Satisfies(ctx) under the gate answers for the same tenant. The
// operator holds tenant.admin, which implies non-reserved permissions only in
// the operator's OWN tenant (tenantA): checked inline in the path tenant
// (tenantB) roles.view must be denied. Without the re-bind the ctx would still
// carry tenantA and the inline check would wrongly pass.
func TestRequireTenantPermission_GateAndInlineDecisionAgree(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	operator := &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.UsersView, p.Meta.TenantAdmin}}
	var inline, inlineTenantDependent bool
	var bound tenancy.ID
	probe := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		inline = authn.Satisfies(r.Context(), p.Users.View)
		inlineTenantDependent = authn.Satisfies(r.Context(), p.Roles.View)
		bound, _ = tenancy.FromContext(r.Context())
	})
	h := authn.RequireTenantPermission(&fakeVerifier{claims: withFreshness(operator)}, alwaysFresh{}, "tenantId", p.Users.View)(probe)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, tenantReq(t, strings.ToUpper(tenantB)))
	if rec.Code != 200 {
		t.Fatalf("gate status %d", rec.Code)
	}
	if inlineTenantDependent {
		t.Error("inline Satisfies(roles.view) passed via tenant.admin in a foreign path tenant: ctx was not re-bound to the path tenant")
	}
	if string(bound) != tenantB {
		t.Fatalf("ctx tenant = %q, want canonical path tenant %q", bound, tenantB)
	}
	if !inline {
		t.Fatal("inline Satisfies disagrees with the gate that just passed")
	}
}

func TestRequireAnyPermission_TenantAdmin(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	s := &sentinel{}
	v := &fakeVerifier{claims: withFreshness(&jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}})}
	h := authn.RequireAnyPermission(v, alwaysFresh{}, p.Users.View, p.Roles.View)(s.handler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != 200 || !s.called {
		t.Fatalf("status %d called %v, want 200 true", rec.Code, s.called)
	}
	s2 := &sentinel{}
	h = authn.RequireAnyPermission(v, alwaysFresh{}, p.Platform.UsersView)(s2.handler())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(t, "Bearer tok"))
	if rec.Code != 403 {
		t.Fatalf("tenant.admin vs reserved: status %d want 403", rec.Code)
	}
}
