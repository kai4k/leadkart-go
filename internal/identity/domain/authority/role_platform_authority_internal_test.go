package authority

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

// A company member never holds the platform-managed role names: neither listed nor implied by
// tenant.admin, whatever the claim carries. Platform authority does.
func TestEffectiveSet_PlatformManagedRoleNamesNeedThePlatformFact(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	in := ActorInput{MembershipID: membership.ID("0190a000-0000-7000-8000-000000000001"), TenantID: tenantOne,
		EffectivePermissions: []string{p.Meta.TenantAdmin, p.Roles.Create}}
	for _, manage := range []bool{false, true} {
		in.MayManageRoles = manage
		a, err := NewActor(in)
		if err != nil {
			t.Fatalf("NewActor: %v", err)
		}
		for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
			if got := a.holds(n, tenantOne); got != manage {
				t.Errorf("manage=%v: holds(%s) = %v", manage, n, got)
			}
		}
		if !a.holds(p.Roles.Assign, tenantOne) {
			t.Errorf("manage=%v: tenant.admin must still imply identity.roles.assign", manage)
		}
	}
}
