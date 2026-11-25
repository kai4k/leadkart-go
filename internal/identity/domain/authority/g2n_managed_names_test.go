package authority_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// g2nActors are the five actors that may be in front of a company member: a company owner, an operator
// with both manage names, a platform super user, an impersonation session and a company actor holding the fact.
func g2nActors(t *testing.T) map[string]authority.Actor {
	t.Helper()
	p := permission.IdentityPermissions
	owner, _ := companyOwnerActor(t, false)
	withFact, _ := companyOwnerActor(t, true)
	return map[string]authority.Actor{
		"company owner": owner,
		"operator": mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true,
			EffectivePermissions: []string{p.Platform.UsersManage, p.Platform.RolesManage}}),
		"super user": mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true}),
		"impersonation": mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(92)), TenantID: tA, IsImpersonation: true,
			EffectivePermissions: []string{p.Meta.TenantAdmin}}),
		"company actor with the fact": withFact,
	}
}

// G2n S1: a managed name newly put on a company member's REVOKED list is a conferral (refused like a
// grant); taking a stale one off is a removal and is never refused.
func TestPlatformManagedNames_RevokedListInACompany(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	clean := mkMember(t, 18, tA, true, "", userR)
	stale := mkMemberOv(t, 19, tA, nil, []string{p.Roles.Update}, userR)
	for name, a := range g2nActors(t) {
		for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
			if err := a.MayChangeOverrides(clean, authority.Overrides{Revoked: []string{n}}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
				t.Errorf("%s adds %s to a company member's revoked list: %v, want ErrGrantExceedsAuthority", name, n, err)
			}
		}
		if err := a.MayChangeOverrides(stale, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}); err != nil {
			t.Errorf("%s clears a stale managed revoke: %v, want nil", name, err)
		}
		// an ordinary revoke is unaffected
		if err := a.MayChangeOverrides(clean, authority.Overrides{Revoked: []string{p.Crm.Leads.Read}}); name == "company owner" && err != nil {
			t.Errorf("%s revokes an ordinary name: %v", name, err)
		}
	}
}

// G2n M1: inside the platform tenant the names are real authority: removing one still needs the ceiling.
func TestPlatformManagedNames_RemovalInThePlatformTenantNeedsTheCeiling(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	// a platform-tenant administrator holding neither managed name (no role fact)
	admin, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(uid(95)), TenantID: tP, Rank: rk(authority.RankAdmin), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Roles.Revoke, p.Users.UpdatePermissions}})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	owner := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(96)), TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin, p.Platform.RolesManage, p.Roles.Update, p.Roles.Manage}})
	carrying := mkRole(61, tP, "Platform Carrier", false, 60, false, p.Roles.Update)
	holder := mkMember(t, 62, tP, true, "", carrying)
	withGrant := mkMemberOv(t, 63, tP, []membership.GrantedOverride{permanent(p.Roles.Manage)}, nil)
	clean := mkMember(t, 64, tP, true, "")
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"admin without the fact revokes a role carrying a managed name", admin.MayRevokeRole(holder, carrying, nil), authority.ErrGrantExceedsAuthority},
		{"admin without the fact removes a managed override", admin.MayChangeOverrides(withGrant, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}), authority.ErrGrantExceedsAuthority},
		{"admin without the fact revokes a managed name", admin.MayChangeOverrides(clean, authority.Overrides{Revoked: []string{p.Roles.Update}}), authority.ErrGrantExceedsAuthority},
		{"platform owner holding the names removes a managed override", owner.MayChangeOverrides(withGrant, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}), nil},
		{"platform owner revokes a role carrying a managed name", owner.MayRevokeRole(holder, carrying, nil), nil},
	} {
		if !errors.Is(tc.err, tc.want) && (tc.want != nil || tc.err != nil) {
			t.Errorf("%s: %v, want %v", tc.name, tc.err, tc.want)
		}
	}
}

// G2n S2: reactivating a role in the platform tenant needs the actor to hold the managed names it confers.
func TestMayReactivateRole_ActorMustHoldTheManagedNames(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	updOnly := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(97)), TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Roles.Update}})
	carrying := mkRole(65, tP, "Platform Manage Carrier", false, 60, false, p.Roles.Manage)
	if err := updOnly.MayReactivateRole(carrying, authority.RoleHolders{}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("actor holding only identity.roles.update reactivates a role carrying identity.roles.manage: %v, want ErrGrantExceedsAuthority", err)
	}
}
