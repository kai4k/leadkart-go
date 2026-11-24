package authority_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// boundary pins every comparison, tenant check and exemption branch of the
// policy: each case below fails if the corresponding line is flipped or deleted.
func TestPolicy_Boundaries(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ta := p.Meta.TenantAdmin

	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, ta)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, ta)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read, p.Roles.Approve)
	mgrR2 := mkRole(4, tA, "Purchase Manager", false, 20, false, p.Roles.Approve)
	seniorMgrR := mkRole(5, tA, "Senior Manager", false, 15, false, p.Roles.Approve)
	tlR := mkRole(6, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Crm.Leads.Read, p.Roles.Approve)
	userR := mkRole(7, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	lvl11TA := mkRole(8, tA, "Almost Manager", false, 11, false, ta)
	superR := mkRole(9, tA, "SuperAdmin", true, 0, true)
	foreignRole := mkRole(10, tB, "Foreign Manager", false, 20, false)
	foreignUserR := mkRole(11, tB, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	foreignMgrR := mkRole(12, tB, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	platformUserR := mkRole(13, tP, "Platform Exec", false, 50, false)
	customSuperR := mkRole(14, tA, "Custom Super", false, 20, true) // a super-admin role that is not system-default

	owner := mkMember(t, 21, tA, true, "", ownerR)
	admin := mkMember(t, 22, tA, true, "", adminR)
	admin2 := mkMember(t, 23, tA, true, "", adminR)
	mgr := mkMember(t, 24, tA, true, "", mgrR)
	tl := mkMember(t, 25, tA, true, mgr.ID(), tlR)
	unmanaged := mkMember(t, 26, tA, true, "", userR)
	reportsToMgr := mkMember(t, 27, tA, true, mgr.ID(), userR)
	reportsToOwner := mkMember(t, 28, tA, true, owner.ID(), userR)
	reportsToAdmin2 := mkMember(t, 29, tA, true, admin2.ID(), userR)
	foreignMgr := mkMember(t, 30, tB, true, "", foreignMgrR)
	foreignUser := mkMember(t, 31, tB, true, "", foreignUserR)
	crossLinked := mkMember(t, 32, tA, true, foreignMgr.ID(), userR) // corrupt data: manager from another tenant
	platformUser := mkMember(t, 33, tP, true, "", platformUserR)
	staleSelf := mkMember(t, 34, tA, true, "", seniorMgrR) // fresh rank 15
	reportsToStaleSelf := mkMember(t, 35, tA, true, staleSelf.ID(), userR)
	superSelf := mkMember(t, 90, tP, true, "")
	opSelf := mkMember(t, 91, tP, true, "")

	act := func(id membership.ID, tid tenant.ID, rank *int, perms []string, mut func(*authority.ActorInput)) authority.Actor {
		in := authority.ActorInput{MembershipID: id, TenantID: tid, Rank: rank, EffectivePermissions: perms}
		if mut != nil {
			mut(&in)
		}
		return mkActor(t, in)
	}
	aOwner := act(owner.ID(), tA, rk(authority.RankOwner), adminEffective(), nil)
	aAdmin := act(admin.ID(), tA, rk(authority.RankAdmin), adminEffective(), nil)
	aTL := act(tl.ID(), tA, rk(authority.RankTeamLead), []string{p.Roles.Approve, p.Crm.Leads.Read}, nil)
	aSuper := act(superSelf.ID(), tP, nil, nil, func(in *authority.ActorInput) { in.IsSuperUser = true })
	aOp := act(opSelf.ID(), tP, rk(authority.RankNone), []string{p.Platform.UsersManage, p.Platform.RolesManage}, func(in *authority.ActorInput) { in.IsPlatformOperator = true })
	aOpRolesOnly := act(opSelf.ID(), tP, rk(authority.RankNone), []string{p.Platform.RolesManage}, func(in *authority.ActorInput) { in.IsPlatformOperator = true })
	aNotOperator := act(opSelf.ID(), tP, rk(authority.RankNone), []string{p.Platform.UsersManage, p.Platform.RolesManage}, nil)
	aImp := act(membership.ID(uid(93)), tA, rk(authority.RankOwner), adminEffective(), func(in *authority.ActorInput) { in.IsImpersonation = true })
	aOwnerNoAdminPerm := act(owner.ID(), tA, rk(authority.RankOwner), []string{p.Crm.Leads.Read}, nil)
	aHoldsReserved := act(admin.ID(), tA, rk(authority.RankAdmin), append(adminEffective(), p.Platform.TenantsManage), nil)
	aStaleSelf := act(staleSelf.ID(), tA, rk(authority.RankTeamLead), []string{p.Roles.Approve}, nil) // rank claim older than the roles

	cases := []decision{
		// strict comparisons
		{"re-level to exactly the actor's rank", aAdmin.MayEditRole(mgrR, 10, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"re-level to one below the actor's rank", aAdmin.MayEditRole(mgrR, 11, authority.RoleHolders{}), nil},
		{"edit a role at exactly the actor's rank", aAdmin.MayEditRole(mkRole(40, tA, "Peer Admin", false, 10, false), authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"edit a role one below the actor's rank", aAdmin.MayEditRole(mkRole(41, tA, "Junior Admin", false, 11, false), authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"create a role at exactly the actor's rank", aAdmin.MayCreateRole(tA, 10, nil, nil), authority.ErrInsufficientRank},
		{"create a role one below the actor's rank", aAdmin.MayCreateRole(tA, 11, nil, nil), nil},
		{"assign a role at exactly the actor's rank", aTL.MayAssignRole(unmanaged, mkRole(42, tA, "Peer TL", false, 30, false)), authority.ErrInsufficientRank},
		{"assign a role one below the actor's rank", aTL.MayAssignRole(unmanaged, mkRole(43, tA, "Junior TL", false, 31, false)), nil},
		{"admin approves a peer admin", aAdmin.MayApprove(admin2, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"admin approves a member one rank below", aAdmin.MayApprove(mkMember(t, 44, tA, true, "", lvl11TA), p.Crm.Leads.Read), nil},
		{"admin acts on a peer admin", aAdmin.MayActOn(admin2), authority.ErrInsufficientRank},
		{"parent at the same level", aAdmin.MaySetRoleParent(mgrR, mgrR2), authority.ErrRoleParentNotEligible},
		{"parent one level above", aAdmin.MaySetRoleParent(mgrR, seniorMgrR), nil},
		{"parent in another tenant", aAdmin.MaySetRoleParent(tlR, foreignRole), authority.ErrNoAuthority},
		{"admin appoints a peer admin as manager", aAdmin.MayReplaceManager(unmanaged, admin2, nil), nil},
		{"manager at the subordinate's rank", aAdmin.MayReplaceManager(unmanaged, mkMember(t, 45, tA, true, "", userR), nil), authority.ErrManagerNotEligible},
		{"unlink an equal-rank manager that is not the actor", aAdmin.MayClearManager(reportsToAdmin2, &admin2), authority.ErrInsufficientRank},
		{"unlink a more senior manager", aTL.MayClearManager(reportsToOwner, &owner), authority.ErrInsufficientRank},
		{"replacing a senior manager with oneself still needs the unlink right", aTL.MayReplaceManager(reportsToOwner, tl, &owner), authority.ErrInsufficientRank},
		// level range
		{"create at level 99", aAdmin.MayCreateRole(tA, 99, nil, nil), nil},
		{"create at level 100", aAdmin.MayCreateRole(tA, 100, nil, nil), authority.ErrInsufficientRank},
		{"create at level -1", aAdmin.MayCreateRole(tA, -1, nil, nil), authority.ErrInsufficientRank},
		{"super creates at level 0 (level 0 is only ever seeded)", aSuper.MayCreateRole(tA, 0, nil, nil), authority.ErrInsufficientRank},
		{"re-level to 150", aAdmin.MayEditRole(mgrR, 150, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"re-level to -5", aAdmin.MayEditRole(mgrR, -5, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"super re-levels to 150", aSuper.MayEditRole(mgrR, 150, authority.RoleHolders{}), authority.ErrInsufficientRank},
		// tenant checks
		{"approve a requester of another tenant", aAdmin.MayApprove(foreignUser, p.Crm.Leads.Read), authority.ErrNoAuthority},
		{"deny a requester of another tenant", aAdmin.MayDeny(foreignUser), authority.ErrNoAuthority},
		{"clear a manager that belongs to another tenant", aAdmin.MayClearManager(crossLinked, &foreignMgr), authority.ErrNoAuthority},
		{"edit another tenant's role", aAdmin.MayEditRole(foreignRole, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrNoAuthority},
		{"assign another tenant's role", aAdmin.MayAssignRole(unmanaged, foreignRole), authority.ErrNoAuthority},
		{"a super user assigns another tenant's role", aSuper.MayAssignRole(unmanaged, foreignRole), authority.ErrNoAuthority},
		{"a super user edits another tenant's role", aSuper.MayEditRole(foreignRole, authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"create a role in another tenant", aAdmin.MayCreateRole(tB, 50, nil, nil), authority.ErrNoAuthority},
		{"act on another tenant's member", aAdmin.MayActOn(foreignUser), authority.ErrNoAuthority},
		// exempt branches
		{"super creates a role in another tenant", aSuper.MayCreateRole(tB, 11, nil, nil), nil},
		{"operator creates a role in another tenant", aOp.MayCreateRole(tB, 11, nil, nil), nil},
		{"operator edits another tenant's senior role", aOp.MayGrantRolePermission(adminR, p.Crm.Leads.Read, authority.RoleHolders{}), nil},
		{"super edits another tenant's senior role", aSuper.MayGrantRolePermission(adminR, p.Crm.Leads.Read, authority.RoleHolders{}), nil},
		{"operator sets a manager more senior than itself", aOp.MayReplaceManager(unmanaged, mgr, nil), nil},
		{"super sets a manager more senior than itself", aSuper.MayReplaceManager(unmanaged, mgr, nil), nil},
		{"operator replaces a more senior manager", aOp.MayClearManager(reportsToOwner, &owner), nil},
		{"super replaces a more senior manager", aSuper.MayClearManager(reportsToOwner, &owner), nil},
		{"operator approves in another tenant", aOp.MayApprove(unmanaged, p.Crm.Leads.Read), nil},
		{"operator assigns the Company Owner role in another tenant", aOp.MayAssignRole(unmanaged, ownerR), authority.ErrOwnershipAuthorityRequired},
		{"operator may not confer a platform-reserved name", aOp.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Platform.TenantsManage)}), authority.ErrGrantExceedsAuthority},
		{"operator may confer anything tenant.admin implies", aOp.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Crm.Leads.ReadAll, p.Tenants.Suspend)}), authority.ErrGrantExceedsAuthority},
		{"operator confers a tenant permission", aOp.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Crm.Leads.ReadAll)}), nil},
		{"operator on its own tenant's member follows rank", aOp.MayActOn(platformUser), authority.ErrInsufficientRank},
		{"operator creates in its own tenant follows rank", aOp.MayCreateRole(tP, 50, nil, nil), authority.ErrInsufficientRank},
		{"operator edits a role in its own tenant follows rank", aOp.MayEditRole(platformUserR, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		// exemption conditions
		{"platform.roles.manage alone exempts", aOpRolesOnly.MayActOn(unmanaged), nil},
		{"platform.users.manage alone exempts", aOp.MayActOn(unmanaged), nil},
		{"users.manage and roles.manage without the operator flag do not exempt", aNotOperator.MayActOn(unmanaged), authority.ErrNoAuthority},
		{"nor for a role edit", aNotOperator.MayEditRole(mgrR, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrNoAuthority},
		{"an operator without either manage permission is not exempt", act(opSelf.ID(), tP, rk(authority.RankNone), []string{p.Platform.TenantsView}, func(in *authority.ActorInput) { in.IsPlatformOperator = true }).MayActOn(unmanaged), authority.ErrNoAuthority},
		// ceiling
		{"an owner whose effective set lacks tenant.admin cannot confer it", aOwnerNoAdminPerm.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(ta)}), authority.ErrGrantExceedsAuthority},
		{"an actor that holds a platform-reserved name still cannot confer it", aHoldsReserved.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Platform.TenantsManage)}), authority.ErrGrantExceedsAuthority},
		{"duplicates in a held set", aTL.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Crm.Leads.Read, p.Crm.Leads.Read)}), nil},
		{"duplicates in an unheld set", aTL.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Crm.Leads.Assign, p.Crm.Leads.Assign)}), authority.ErrGrantExceedsAuthority},
		{"a held name next to an unheld one", aTL.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(p.Crm.Leads.Read, p.Crm.Leads.Assign)}), authority.ErrGrantExceedsAuthority},
		{"an owner confers tenant.admin", aOwner.MayChangeOverrides(unmanaged, authority.Overrides{Granted: grants(ta)}), nil},
		{"impersonation assigns a level-11 role containing tenant.admin", aImp.MayAssignRole(unmanaged, lvl11TA), authority.ErrGrantExceedsAuthority},
		{"impersonation assigns a level-11 role without it", aImp.MayAssignRole(unmanaged, mkRole(46, tA, "Plain 11", false, 11, false)), nil},
		// self and super-admin rules
		{"super self-approval", aSuper.MayApprove(superSelf, p.Crm.Leads.Read), authority.ErrSelfTarget},
		{"exempt operator on its own membership", aOp.MayActOn(opSelf), authority.ErrSelfTarget},
		{"exempt operator approving itself", aOp.MayApprove(opSelf, p.Crm.Leads.Read), authority.ErrSelfTarget},
		{"impersonation denies", aImp.MayDeny(unmanaged), authority.ErrImpersonationNotAllowed},
		{"impersonation approves", aImp.MayApprove(unmanaged, p.Crm.Leads.Read), authority.ErrImpersonationNotAllowed},
		{"a super-admin role cannot be edited by an owner", aOwner.MayEditRole(customSuperR, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"a super user edits a super-admin role", aSuper.MayEditRole(customSuperR, authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"a system-default super-admin role is immutable even for a super user", aSuper.MayEditRole(superR, authority.NoLevelChange, authority.RoleHolders{}), role.ErrSystemDefault},
		{"an operator cannot edit a super-admin role", aOp.MayEditRole(customSuperR, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"an owner cannot assign a super-admin role", aOwner.MayAssignRole(unmanaged, superR), authority.ErrGrantExceedsAuthority},
		{"a super user assigns a super-admin role", aSuper.MayAssignRole(unmanaged, superR), nil},
		// self shortcuts: the rank claim may be older than the roles
		{"appointing oneself passes though the claim says a lower rank", aStaleSelf.MayReplaceManager(unmanaged, staleSelf, nil), nil},
		{"unlinking oneself passes though the claim says a lower rank", aStaleSelf.MayClearManager(reportsToStaleSelf, &staleSelf), nil},
		{"appointing a more senior non-self is refused", aStaleSelf.MayReplaceManager(unmanaged, mgr, nil), authority.ErrInsufficientRank},
		{"manager with rank above the claim is refused", aTL.MayReplaceManager(reportsToMgr, mgr, &mgr), authority.ErrInsufficientRank},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

func TestMayActOn_SuperAdminTargetOnlyBySuperUser(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	superR := mkRole(2, tA, "SuperAdmin", true, 0, true)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	superMember := mkMember(t, 12, tA, true, "", superR)
	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	for _, m := range mismatches([]decision{
		{"owner on a super-admin member", aOwner.MayActOn(superMember), authority.ErrInsufficientRank},
		{"operator on a super-admin member", aOp.MayActOn(superMember), authority.ErrInsufficientRank},
		{"super on a super-admin member", aSuper.MayActOn(superMember), nil},
	}) {
		t.Error(m)
	}
}

func TestMemberOf_IgnoresDeletedRolesForFlagsAndRank(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	userR := mkRole(1, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	goneOwner := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(2)), TenantID: tA, Name: role.SystemRoles.Tenant.CompanyOwner, IsSystemDefault: true,
		HierarchyLevel: 0, Permissions: []*permission.Permission{permission.FromConstant(p.Meta.TenantAdmin)}, CreatedAt: t0})
	goneSuper := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(3)), TenantID: tA, Name: "SuperAdmin", IsSuperAdmin: true, CreatedAt: t0})
	stray := mkRole(4, tA, "Stray", false, 40, false)

	m := mkMembership(11, tA, true, "", userR, goneOwner, goneSuper)
	got, err := authority.MemberOf(m, []*role.Role{userR, goneOwner, goneSuper}, t0, activePerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	if got.IsOwner() || got.IsSuperAdmin() || got.Rank() != authority.RankUser {
		t.Errorf("deleted roles leaked: owner=%v super=%v rank=%d", got.IsOwner(), got.IsSuperAdmin(), got.Rank())
	}
	// Same number of roles as assigned, but one is not an assigned role.
	if _, err := authority.MemberOf(m, []*role.Role{userR, goneOwner, stray}, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("a stray role standing in for an assigned one: %v, want no authority", err)
	}
	inactive := mkMembership(12, tA, false, "", userR)
	if got, err := authority.MemberOf(inactive, []*role.Role{userR}, t0, activePerson()); err != nil || got.IsActive() {
		t.Errorf("inactive membership projected active: %+v, %v", got, err)
	}
	active := mkMembership(13, tA, true, "", userR)
	if got, err := authority.MemberOf(active, []*role.Role{userR}, t0, activePerson()); err != nil || !got.IsActive() {
		t.Errorf("active membership projected inactive: %+v, %v", got, err)
	}
}

func TestNewActor_RejectsInvalidCombinationsAndIDs(t *testing.T) {
	t.Parallel()
	id := membership.ID(uid(1))
	for name, in := range map[string]authority.ActorInput{
		"impersonation with super user": {MembershipID: id, TenantID: tA, IsImpersonation: true, IsSuperUser: true},
		"impersonation with operator":   {MembershipID: id, TenantID: tA, IsImpersonation: true, IsPlatformOperator: true},
		"empty tenant id":               {MembershipID: id, TenantID: ""},
		"empty membership id":           {MembershipID: "", TenantID: tA},
		"nil-uuid tenant id":            {MembershipID: id, TenantID: "00000000-0000-0000-0000-000000000000"},
		"padded tenant id":              {MembershipID: id, TenantID: tenant.ID(" " + string(tA))},
	} {
		_, err := authority.NewActor(in)
		if !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("%s: %v, want no authority", name, err)
		}
	}
	hyphenless := strings.ReplaceAll(string(tA), "-", "")
	a, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(strings.ToUpper(strings.ReplaceAll(uid(1), "-", ""))), TenantID: tenant.ID(hyphenless)})
	if err != nil || a.TenantID() != tA || a.MembershipID() != id {
		t.Errorf("hyphenless upper-case ids: %v, %s / %s", err, a.TenantID(), a.MembershipID())
	}
}

func TestRank_BoundariesOfTierClampAndDelta(t *testing.T) {
	t.Parallel()
	tiers := map[int]authority.Tier{0: authority.TierOwner, 1: authority.TierAdmin, 10: authority.TierAdmin, 11: authority.TierManager,
		20: authority.TierManager, 21: authority.TierTeamLead, 30: authority.TierTeamLead, 31: authority.TierUser, 99: authority.TierUser}
	for lvl, want := range tiers {
		if got := authority.Rank(lvl).Tier(); got != want {
			t.Errorf("Rank(%d).Tier() = %s, want %s", lvl, got, want)
		}
	}
	for in, want := range map[int]authority.Rank{-1: authority.RankNone, 0: authority.RankOwner, 1: 1, 98: 98, 99: authority.RankNone, 100: authority.RankNone} {
		if got := authority.ClampRank(in); got != want {
			t.Errorf("ClampRank(%d) = %d, want %d", in, got, want)
		}
	}
	for name, c := range map[string]struct {
		before, after []string
		want          string
	}{
		"nil and nil":      {nil, nil, "[]"},
		"identical":        {[]string{"a", "b"}, []string{"b", "a"}, "[]"},
		"disjoint":         {[]string{"b", "a"}, []string{"d", "c"}, "[a b c d]"},
		"added only":       {nil, []string{"x"}, "[x]"},
		"removed only":     {[]string{"x"}, nil, "[x]"},
		"duplicates fold":  {[]string{"a", "a"}, []string{"a", "b", "b"}, "[b]"},
		"one side differs": {[]string{"a", "b"}, []string{"b", "c"}, "[a c]"},
	} {
		got := authority.Delta(c.before, c.after)
		if got == nil || fmt.Sprint(got) != c.want {
			t.Errorf("Delta %s = %v (nil=%v), want %s and non-nil", name, got, got == nil, c.want)
		}
	}
	if got := authority.PermissionNames(nil); got == nil || len(got) != 0 {
		t.Errorf("PermissionNames(nil) = %v, want empty non-nil", got)
	}
}
