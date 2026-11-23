package authority_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Owner ruling 2026-11-21: "SuperAdmin and Platform Team, Company Admin and
// Company Owner are allowed to add users only." The permission that adds users
// is held and conferred only at the Administrator tier or above.

// The set is users.create plus the two company commerce settings permissions
// (owner ruling 2026-11-23, ADR 0070: only the company's Owner / Admin tier
// changes them).
func TestAdminTierOnly_TheSetIsExactlyUsersCreateAndTheCompanySettingsWrites(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	want := []string{p.Users.Create, p.CompanySettings.Manage, p.CompanySettings.TaxManage}
	if got := authority.AdminTierOnlyPermissions(); !slices.Equal(got, want) {
		t.Fatalf("admin-tier-only set = %v, want exactly %v", got, want)
	}
	for _, perm := range permission.All() {
		if got, want := authority.IsAdminTierOnly(perm.Name()), slices.Contains(want, perm.Name()); got != want {
			t.Errorf("IsAdminTierOnly(%s) = %v, want %v", perm.Name(), got, want)
		}
	}
	if authority.IsAdminTierOnly(p.CompanySettings.Read) {
		t.Error("company_settings.read is not admin-tier-only: managers read the GST list")
	}
}

// TestMayCreateMembers_OnlyOwnerAdminAndOperators: every tier below Administrator
// is refused even when it holds the permission; the platform side passes by the
// platform rule, never by tenant tier.
func TestMayCreateMembers_OnlyOwnerAdminAndOperators(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	holds := []string{p.Users.Create}
	at := func(rank authority.Rank) authority.Actor {
		return mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(40)), TenantID: tA, Rank: rk(rank), EffectivePermissions: holds})
	}
	var zero authority.Actor
	op := func(perms ...string) authority.Actor {
		return mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: perms})
	}
	noRank := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(41)), TenantID: tA, EffectivePermissions: holds})
	cases := []decision{
		{"owner", at(authority.RankOwner).MayCreateMembers(tA), nil},
		{"administrator", at(authority.RankAdmin).MayCreateMembers(tA), nil},
		{"office administrator at level 10", at(10).MayCreateMembers(tA), nil},
		{"senior manager holding it", at(15).MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"manager holding it", at(authority.RankManager).MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"team lead holding it", at(authority.RankTeamLead).MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"executive holding it", at(authority.RankUser).MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"member with no role holding it", at(authority.RankNone).MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"token without a rank claim", noRank.MayCreateMembers(tA), authority.ErrAdminTierOnly},
		{"admin in another tenant", at(authority.RankAdmin).MayCreateMembers(tB), authority.ErrNoAuthority},
		{"zero actor", zero.MayCreateMembers(tA), authority.ErrNoAuthority},
		{"super user in a customer tenant", mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true}).MayCreateMembers(tA), nil},
		{"operator with platform.users.create", op(p.Platform.UsersCreate).MayCreateMembers(tA), nil},
		{"operator with platform.users.manage", op(p.Platform.UsersManage).MayCreateMembers(tA), nil},
		{"operator with platform.users.create in the platform tenant", op(p.Platform.UsersCreate).MayCreateMembers(tP), nil},
		{"operator with neither", op(p.Platform.TenantsView).MayCreateMembers(tA), authority.ErrNoAuthority},
		{"impersonation session", mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, IsImpersonation: true, EffectivePermissions: adminEffective()}).MayCreateMembers(tA), nil},
		{"admin without the permission", mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(42)), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: []string{p.Users.View}}).MayCreateMembers(tA), authority.ErrNoAuthority},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestAdminTierOnly_NoRouteBelowTheTierConfersIt: role create / edit / grant /
// replace / assign, overrides, requests and approvals.
func TestAdminTierOnly_NoRouteBelowTheTierConfersIt(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	create := p.Users.Create
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	staleMgrR := mkRole(6, tA, "Stale Manager", false, 20, false, create) // data from before the ruling
	staleAdminLevelR := mkRole(7, tA, "Admin-level Creator", false, 10, false, create)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	user := mkMember(t, 18, tA, true, "", userR)
	noRole := mkMember(t, 19, tA, true, "")
	adminTarget := mkMember(t, 20, tA, true, "", mkRole(8, tA, "Admin-tier custom", false, 5, false))
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)
	aMgr := actorOf(t, mgr, authority.RankManager, p.Roles.Approve, p.Crm.Leads.Read)
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage, p.Platform.RolesManage}})
	none := authority.RoleHolders{}
	cases := []decision{
		// a role below the tier cannot contain it
		{"create a manager-level role with it", aOwner.MayCreateRole(tA, 20, []string{create}, nil), authority.ErrAdminTierOnly},
		{"create an executive-level role with it (super user)", aSuper.MayCreateRole(tA, 50, []string{create}, nil), authority.ErrAdminTierOnly},
		{"create a level-5 role with it", aOwner.MayCreateRole(tA, 5, []string{create}, nil), nil},
		{"grant it to a manager-level role", aOwner.MayGrantRolePermission(mgrR, create, none), authority.ErrAdminTierOnly},
		{"grant it to a manager-level role (operator)", aOp.MayGrantRolePermission(mgrR, create, none), authority.ErrAdminTierOnly},
		{"replace a manager-level role's set with it", aOwner.MayReplaceRolePermissions(mgrR, []string{p.Crm.Leads.Read, create}, none), authority.ErrAdminTierOnly},
		{"replace an executive-level role's set with it", aAdmin.MayReplaceRolePermissions(userR, []string{create}, none), authority.ErrAdminTierOnly},
		{"remove it from a stale manager-level role", aOwner.MayReplaceRolePermissions(staleMgrR, []string{}, none), nil},
		{"re-level a role that contains it to manager", aOwner.MayEditRole(staleAdminLevelR, 20, none), authority.ErrAdminTierOnly},
		{"re-level a role that contains it within the admin tier", aOwner.MayEditRole(staleAdminLevelR, 8, none), nil},
		{"assign a stale manager-level role that contains it", aOwner.MayAssignRole(user, staleMgrR), authority.ErrAdminTierOnly},
		// overrides on a member below the tier
		{"override-grant it to a manager", aOwner.MayChangeOverrides(mgr, authority.Overrides{Granted: grants(create)}), authority.ErrAdminTierOnly},
		{"override-grant it to an executive", aAdmin.MayChangeOverrides(user, authority.Overrides{Granted: grants(create)}), authority.ErrAdminTierOnly},
		{"override-grant it to a member with no role", aOwner.MayChangeOverrides(noRole, authority.Overrides{Granted: grants(create)}), authority.ErrAdminTierOnly},
		{"override-grant it to a member below the tier (super user)", aSuper.MayChangeOverrides(user, authority.Overrides{Granted: grants(create)}), authority.ErrAdminTierOnly},
		{"override-grant it to an admin-tier member", aOwner.MayChangeOverrides(adminTarget, authority.Overrides{Granted: grants(create)}), nil},
		// requests and approvals
		{"a manager asks for it", aMgr.MayRequest(create), authority.ErrAdminTierOnly},
		{"an executive asks for it", actorOf(t, user, authority.RankUser, p.Meta.RequestPermissionElevation).MayRequest(create), authority.ErrAdminTierOnly},
		{"an administrator asks for something else", aAdmin.MayRequest(p.Crm.Leads.Read), nil},
		{"an administrator asks for it", aAdmin.MayRequest(create), nil},
		{"an administrator approves it for a manager", aAdmin.MayApprove(mgr, create), authority.ErrAdminTierOnly},
		{"an owner approves it for an executive", aOwner.MayApprove(user, create), authority.ErrAdminTierOnly},
		{"an operator approves it for an executive", aOp.MayApprove(user, create), authority.ErrAdminTierOnly},
		{"an owner approves it for an admin-tier requester", aOwner.MayApprove(adminTarget, create), nil},
		{"an administrator still approves another day-to-day name", aAdmin.MayApprove(mgr, p.Crm.Leads.Assign), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}
