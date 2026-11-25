package authority_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// G2o B1: "is the target the platform tenant?" is decided by the TARGET tenant, never by who the actor
// is. In the platform tenant a managed name is real authority: lifting a managed revoke confers it.
func TestPlatformManagedNames_PlatformTenantIsDecidedByTheTarget(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	staff := mkMemberOv(t, 71, tP, nil, []string{p.Roles.Manage})
	lift := authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}

	// an impersonation session whose token names the platform tenant (G2n review B1)
	imp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(72)), TenantID: tP, IsImpersonation: true,
		PlatformTenantID: tP, EffectivePermissions: []string{p.Meta.TenantAdmin}})
	// a company super user acting in the platform tenant (G2n review m4)
	coSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(73)), TenantID: tA, IsSuperUser: true, PlatformTenantID: tP})
	// the same two against a company: stale revokes stay liftable there
	companyStale := mkMemberOv(t, 74, tA, nil, []string{p.Roles.Manage})

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"impersonation in the platform tenant cannot lift a managed revoke", imp.MayChangeOverrides(staff, lift), authority.ErrGrantExceedsAuthority},
		{"company super user in the platform tenant cannot lift a managed revoke", coSuper.MayChangeOverrides(staff, lift), authority.ErrGrantExceedsAuthority},
		{"company super user in a company lifts a stale managed revoke", coSuper.MayChangeOverrides(companyStale, lift), nil},
	} {
		if !errors.Is(tc.err, tc.want) && (tc.want != nil || tc.err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, tc.err, tc.want)
		}
	}
	impCompany := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(75)), TenantID: tA, IsImpersonation: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin}})
	if err := impCompany.MayChangeOverrides(companyStale, lift); err != nil {
		t.Errorf("impersonation in a company lifts a stale managed revoke: %v, want nil", err)
	}
}

// G2n m1: keeping a stale managed revoke while changing another override is not a conferral.
func TestPlatformManagedNames_KeepingAStaleRevokeWhileEditingOthers(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	stale := mkMemberOv(t, 76, tA, nil, []string{p.Roles.Update})
	keep := authority.Overrides{Granted: []authority.Override{{Name: p.Crm.Leads.Read}}, Revoked: []string{p.Roles.Update}}
	owner, _ := companyOwnerActor(t, false)
	if err := owner.MayChangeOverrides(stale, keep); err != nil {
		t.Errorf("owner keeps a stale managed revoke and grants crm.leads.read: %v, want nil", err)
	}
}

// G2n m2: in the platform tenant a holder of identity.roles.manage only through a time-bound grant
// cannot lift a colleague's managed revoke (that would make the name permanent for the colleague).
func TestPlatformManagedNames_TimedHolderCannotLiftInThePlatformTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	id := membership.ID(uid(77))
	owner := mkActor(t, authority.ActorInput{MembershipID: id, TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true, MayManageRoles: true,
		EffectivePermissions: []string{p.Users.UpdatePermissions, p.Platform.RolesManage, p.Roles.Update, p.Roles.Manage}})
	ownerR := mkRole(79, tP, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	userR := mkRole(80, tP, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	fresh := mkMemberOv(t, 77, tP, []membership.GrantedOverride{{Permission: permission.FromConstant(p.Roles.Manage), ExpiresAt: t0.Add(48 * time.Hour)}}, nil, ownerR)
	timed := owner.NarrowedBy(fresh, []string{p.Users.UpdatePermissions, p.Platform.RolesManage, p.Roles.Update, p.Roles.Manage},
		[]string{p.Users.UpdatePermissions, p.Platform.RolesManage, p.Roles.Update})
	colleague := mkMemberOv(t, 78, tP, nil, []string{p.Roles.Manage}, userR)
	if err := timed.MayChangeOverrides(colleague, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("timed holder lifts a colleague's managed revoke in the platform tenant: %v, want ErrGrantExceedsAuthority", err)
	}
}
