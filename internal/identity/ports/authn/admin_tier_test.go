package authn_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Owner ruling 2026-11-21: identity.users.create is satisfied, and listed in
// effective_permissions, only for the Administrator tier or above, the platform
// rule (super user, operator's platform.users.create / manage) and an
// impersonation session. A grant below the tier (an override or a role stored
// before the ruling, or tenant.admin conferred on a manager) is inert.
func TestSatisfies_UsersCreateIsAdminTierOnly(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	rank := func(n int) *int { return &n }
	operator := func(perms ...string) *jwt.Claims {
		return &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: perms}
	}
	cases := []struct {
		name   string
		claims *jwt.Claims
		want   bool
	}{
		{"owner (tenant.admin, rank 0)", &jwt.Claims{TenantID: tenantA, Rank: rank(0), Permissions: []string{p.Meta.TenantAdmin}}, true},
		{"administrator (tenant.admin, rank 10)", &jwt.Claims{TenantID: tenantA, Rank: rank(10), Permissions: []string{p.Meta.TenantAdmin}}, true},
		{"admin-tier role with the explicit grant", &jwt.Claims{TenantID: tenantA, Rank: rank(5), Permissions: []string{p.Users.Create}}, true},
		{"senior manager with the explicit grant", &jwt.Claims{TenantID: tenantA, Rank: rank(15), Permissions: []string{p.Users.Create}}, false},
		{"manager with the explicit grant", &jwt.Claims{TenantID: tenantA, Rank: rank(20), Permissions: []string{p.Users.Create}}, false},
		{"manager with tenant.admin", &jwt.Claims{TenantID: tenantA, Rank: rank(20), Permissions: []string{p.Meta.TenantAdmin}}, false},
		{"executive with the explicit grant", &jwt.Claims{TenantID: tenantA, Rank: rank(50), Permissions: []string{p.Users.Create}}, false},
		{"token without a rank claim", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Users.Create}}, false},
		{"rank claim out of range", &jwt.Claims{TenantID: tenantA, Rank: rank(-3), Permissions: []string{p.Users.Create}}, false},
		{"super user", &jwt.Claims{TenantID: tenantA, IsSuperUser: true}, true},
		{"impersonation session", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}, true},
		{"operator with platform.users.create", operator(p.Platform.UsersCreate), true},
		{"operator with platform.users.manage", operator(p.Platform.UsersManage), true},
		{"operator with platform.users.view only", operator(p.Platform.UsersView), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := tenancy.WithID(authn.WithClaims(t.Context(), tc.claims), tenancy.ID(tenantA))
			if got := authn.Satisfies(ctx, p.Users.Create); got != tc.want {
				t.Errorf("Satisfies(users.create) = %v, want %v", got, tc.want)
			}
			// effective_permissions comes from the same decision.
			if got := slices.Contains(authn.EffectivePermissions(tc.claims), p.Users.Create); got != tc.want {
				t.Errorf("effective_permissions contains users.create = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rest of tenant.admin's implication is untouched below the tier.
func TestSatisfies_BelowTheTierEverythingElseTenantAdminImpliesStays(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	n := 20
	c := &jwt.Claims{TenantID: tenantA, Rank: &n, Permissions: []string{p.Meta.TenantAdmin}}
	ctx := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(tenantA))
	for _, name := range []string{p.Users.Update, p.Users.View, p.Roles.Assign, p.Crm.Leads.Read} {
		if !authn.Satisfies(ctx, name) {
			t.Errorf("a manager holding tenant.admin lost %s", name)
		}
	}
}

// Owner ruling 2026-11-23 (ADR 0070): the company commerce settings are changed
// only by the company's Owner / Admin tier. company_settings.manage and
// company_settings.tax.manage follow the same tier rule as identity.users.create:
// a grant below the tier is inert and absent from effective_permissions, while
// company_settings.read is not tier-limited.
func TestSatisfies_CompanySettingsWritesAreAdminTierOnly(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	rank := func(n int) *int { return &n }
	cases := []struct {
		name      string
		claims    *jwt.Claims
		wantWrite bool
		wantRead  bool
	}{
		{"owner (tenant.admin, rank 0)", &jwt.Claims{TenantID: tenantA, Rank: rank(0), Permissions: []string{p.Meta.TenantAdmin}}, true, true},
		{"administrator (tenant.admin, rank 10)", &jwt.Claims{TenantID: tenantA, Rank: rank(10), Permissions: []string{p.Meta.TenantAdmin}}, true, true},
		{"admin-tier role with explicit grants", &jwt.Claims{TenantID: tenantA, Rank: rank(5), Permissions: []string{p.CompanySettings.Manage, p.CompanySettings.TaxManage, p.CompanySettings.Read}}, true, true},
		{"manager with explicit grants", &jwt.Claims{TenantID: tenantA, Rank: rank(20), Permissions: []string{p.CompanySettings.Manage, p.CompanySettings.TaxManage, p.CompanySettings.Read}}, false, true},
		{"manager with tenant.admin", &jwt.Claims{TenantID: tenantA, Rank: rank(20), Permissions: []string{p.Meta.TenantAdmin}}, false, true},
		{"executive with explicit grants", &jwt.Claims{TenantID: tenantA, Rank: rank(50), Permissions: []string{p.CompanySettings.Manage, p.CompanySettings.TaxManage}}, false, false},
		{"token without a rank claim", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.CompanySettings.Manage}}, false, false},
		{"impersonation session", &jwt.Claims{TenantID: tenantA, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op"}}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := tenancy.WithID(authn.WithClaims(t.Context(), tc.claims), tenancy.ID(tenantA))
			for _, name := range []string{p.CompanySettings.Manage, p.CompanySettings.TaxManage} {
				if got := authn.Satisfies(ctx, name); got != tc.wantWrite {
					t.Errorf("Satisfies(%s) = %v, want %v", name, got, tc.wantWrite)
				}
				if got := slices.Contains(authn.EffectivePermissions(tc.claims), name); got != tc.wantWrite {
					t.Errorf("effective_permissions contains %s = %v, want %v", name, got, tc.wantWrite)
				}
			}
			if got := authn.Satisfies(ctx, p.CompanySettings.Read); got != tc.wantRead {
				t.Errorf("Satisfies(company_settings.read) = %v, want %v", got, tc.wantRead)
			}
		})
	}
}

// Owner ruling 2026-11-25 (ADR 0073 addendum): identity.roles.{create,update,manage} are LeadKart
// platform authority. No company member holds them, whatever they carry (tenant.admin, an explicit
// grant, a super-admin role of the company); an impersonation session and a platform super user do;
// identity.roles.assign is unchanged.
func TestSatisfies_RoleManagementIsPlatformAuthority(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	owner, admin := 0, 10
	managed := []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage}
	cases := []struct {
		name   string
		claims *jwt.Claims
		want   bool
	}{
		{"owner with tenant.admin", &jwt.Claims{TenantID: tenantA, Rank: &owner, Permissions: []string{p.Meta.TenantAdmin}}, false},
		{"administrator with tenant.admin", &jwt.Claims{TenantID: tenantA, Rank: &admin, Permissions: []string{p.Meta.TenantAdmin}}, false},
		{"administrator with the explicit grants", &jwt.Claims{TenantID: tenantA, Rank: &admin, Permissions: managed}, false},
		{"a company super-admin role", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", Rank: &owner, IsSuperUser: true, Permissions: []string{p.Meta.TenantAdmin}}, false},
		{"impersonation session", &jwt.Claims{TenantID: tenantA, Rank: &admin, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op", SessionID: "s", Reason: "support"}}, true},
		{"platform super user", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, Rank: &owner, IsSuperUser: true}, true},
		// S1 (G2l): LeadKart staff manage roles everywhere, LeadKart's own platform tenant included
		// (what the platform tenant's owner and admins could do before the ruling).
		{"platform-tenant owner (tenant.admin)", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Rank: &owner, Permissions: []string{p.Meta.TenantAdmin}}, true},
		{"platform-tenant administrator (explicit grants)", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Rank: &admin, Permissions: managed}, true},
		{"platform-tenant member without any grant", &jwt.Claims{TenantID: tenantA, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Rank: &admin, Permissions: []string{p.Roles.View}}, false},
		{"is_platform claim on a company slug", &jwt.Claims{TenantID: tenantA, TenantSlug: "acme", IsPlatform: true, Rank: &owner, Permissions: []string{p.Meta.TenantAdmin}}, false},
	}
	for _, tc := range cases {
		ctx := tenancy.WithID(authn.WithClaims(t.Context(), tc.claims), tenancy.ID(tenantA))
		for _, name := range managed {
			if got := authn.Satisfies(ctx, name); got != tc.want {
				t.Errorf("%s: Satisfies(%s) = %v, want %v", tc.name, name, got, tc.want)
			}
		}
	}
	// assign is a company power and stays so: an owner, an administrator and a company super-admin
	// role keep it.
	for _, c := range []struct {
		name   string
		claims *jwt.Claims
	}{{cases[0].name, cases[0].claims}, {cases[1].name, cases[1].claims}, {cases[3].name, cases[3].claims}} {
		ctx := tenancy.WithID(authn.WithClaims(t.Context(), c.claims), tenancy.ID(tenantA))
		if !authn.Satisfies(ctx, p.Roles.Assign) {
			t.Errorf("%s lost identity.roles.assign", c.name)
		}
	}
	// A platform operator reaches a company through the platform equivalent.
	op := &jwt.Claims{TenantID: tenantB, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.RolesManage}}
	ctx := tenancy.WithID(authn.WithClaims(t.Context(), op), tenancy.ID(tenantA))
	for _, name := range managed {
		if !authn.Satisfies(ctx, name) {
			t.Errorf("platform operator with platform.roles.manage cannot satisfy %s in a company", name)
		}
	}
	// Capabilities: the owner effective set no longer lists them, assign stays.
	eff := authn.EffectivePermissions(cases[0].claims)
	for _, name := range managed {
		if slices.Contains(eff, name) {
			t.Errorf("owner effective_permissions lists %s", name)
		}
	}
	if !slices.Contains(eff, p.Roles.Assign) {
		t.Error("owner effective_permissions lost identity.roles.assign")
	}
}

// The domain actor carries the same decision as a fact (S3): AuthorityFrom computes it with the one rule.
func TestAuthorityFrom_MayManageRolesFollowsPlatformAuthority(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	owner := 0
	for _, tc := range []struct {
		name   string
		claims *jwt.Claims
		want   bool
	}{
		{"company owner", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Rank: &owner, Permissions: []string{p.Meta.TenantAdmin}}, false},
		{"company super-admin role", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, TenantSlug: "acme", Rank: &owner, IsSuperUser: true}, false},
		{"impersonation session", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, Permissions: []string{p.Meta.TenantAdmin}, Act: &jwt.ActClaim{Sub: "op", SessionID: "s", Reason: "support"}}, true},
		{"platform-tenant owner", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Rank: &owner, Permissions: []string{p.Meta.TenantAdmin}}, true},
		{"operator with platform.roles.manage", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, TenantSlug: authn.PlatformTenantSlug, IsPlatform: true, Permissions: []string{p.Platform.RolesManage}}, true},
		{"platform super user", &jwt.Claims{TenantID: tenantA, MembershipID: selfMem, TenantSlug: authn.PlatformTenantSlug, Rank: &owner, IsSuperUser: true}, true},
	} {
		if got := authn.AuthorityFrom(authn.WithClaims(t.Context(), tc.claims)).MayManageRoles(); got != tc.want {
			t.Errorf("%s: MayManageRoles = %v, want %v", tc.name, got, tc.want)
		}
	}
}
