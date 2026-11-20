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
	for _, name := range []string{p.Users.Update, p.Users.View, p.Roles.Create, p.Crm.Leads.Read} {
		if !authn.Satisfies(ctx, name) {
			t.Errorf("a manager holding tenant.admin lost %s", name)
		}
	}
}
