package command

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The JWT permission claim (and so /me.permissions) never lists the platform-managed role names for a
// company member; the platform tenant keeps what it holds (ADR 0073 addendum).
func TestClaimPermissionNames_PlatformManagedRoleNamesOnlyInThePlatformTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	perms := []*permission.Permission{
		permission.FromConstant(p.Roles.Update), permission.FromConstant(p.Roles.Create),
		permission.FromConstant(p.Roles.Manage), permission.FromConstant(p.Roles.Assign),
	}
	company := claimPermissionNames(perms, "acme")
	if !slices.Equal(company, []string{p.Roles.Assign}) {
		t.Errorf("company claim = %v, want only identity.roles.assign", company)
	}
	platform := claimPermissionNames(perms, tenant.PlatformSlug)
	if len(platform) != 4 {
		t.Errorf("platform-tenant claim = %v, want all four", platform)
	}
}
