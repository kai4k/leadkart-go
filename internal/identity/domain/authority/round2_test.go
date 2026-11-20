package authority_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// actorOf builds an actor for member m with the rank and effective permissions given.
func actorOf(t *testing.T, m authority.Member, rank authority.Rank, perms ...string) authority.Actor {
	t.Helper()
	return mkActor(t, authority.ActorInput{MembershipID: m.ID(), TenantID: m.TenantID(), Rank: rk(rank), EffectivePermissions: perms})
}

// TestMayCreateRole_TenantMustCanonicalise: I1 — an unparseable or nil tenant
// id never exempts anybody and never reaches the permission ceiling, for every
// actor (super users included); an upper-case spelling of the actor's own tenant is that tenant.
func TestMayCreateRole_TenantMustCanonicalise(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	admin := mkMember(t, 13, tA, true, "", adminR)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)
	var zero authority.Actor
	nilUUID := tenant.ID("00000000-0000-0000-0000-000000000000")

	var cases []decision
	for name, a := range map[string]authority.Actor{"operator": op, "super": super, "admin": aAdmin, "zero": zero} {
		for _, bad := range []tenant.ID{"", nilUUID, "garbage", tenant.ID(" " + tA.String()), tenant.ID(tA.String() + "\n")} {
			cases = append(cases, decision{name + " creates in " + string(bad), a.MayCreateRole(bad, 11, []string{p.Meta.TenantAdmin}, nil), authority.ErrNoAuthority})
		}
	}
	cases = append(cases,
		decision{"operator, upper-case spelling of its own tenant, is not exempt", op.MayCreateRole(tenant.ID(strings.ToUpper(tP.String())), 11, nil, nil), authority.ErrInsufficientRank},
		decision{"operator, canonical other tenant, is exempt", op.MayCreateRole(tB, 11, nil, nil), nil},
		decision{"super, upper-case other tenant, is exempt", super.MayCreateRole(tenant.ID(strings.ToUpper(tB.String())), 11, nil, nil), nil},
	)
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestRoleDecisions_GarbageRoleTenantNeverExempts: the exemption helpers need a
// canonical tenant on the role too (a corrupt row is not "another tenant").
func TestRoleDecisions_GarbageRoleTenantNeverExempts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	var cases []decision
	for _, bad := range []tenant.ID{"", "garbage", "00000000-0000-0000-0000-000000000000"} {
		r := mkRole(41, bad, "Corrupt", false, 50, false, p.Crm.Leads.Read)
		for name, a := range map[string]authority.Actor{"operator": op, "super": super} {
			cases = append(cases,
				decision{name + " edits a role of tenant " + string(bad), a.MayEditRole(r, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrNoAuthority},
				decision{name + " replaces permissions of a role of tenant " + string(bad), a.MayReplaceRolePermissions(r, nil, authority.RoleHolders{}), authority.ErrNoAuthority},
				decision{name + " deletes a role of tenant " + string(bad), a.MayDeleteRole(r, authority.RoleHolders{}), authority.ErrNoAuthority},
			)
		}
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestLevelZeroIsNeverMintedByAnyActor: I2 — create and re-level target [1, 99] for everybody.
func TestLevelZeroIsNeverMintedByAnyActor(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})

	cases := []decision{
		{"owner creates level 0", aOwner.MayCreateRole(tA, 0, nil, nil), authority.ErrInsufficientRank},
		{"super creates level 0", aSuper.MayCreateRole(tA, 0, nil, nil), authority.ErrInsufficientRank},
		{"operator creates level 0 elsewhere", aOp.MayCreateRole(tA, 0, nil, nil), authority.ErrInsufficientRank},
		{"owner creates level 1", aOwner.MayCreateRole(tA, 1, nil, nil), nil},
		{"super creates level 1", aSuper.MayCreateRole(tA, 1, nil, nil), nil},
		{"owner re-levels to 0", aOwner.MayEditRole(mgrR, 0, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"super re-levels to 0", aSuper.MayEditRole(mgrR, 0, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"operator re-levels to 0", aOp.MayEditRole(mgrR, 0, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"owner re-levels to 1", aOwner.MayEditRole(mgrR, 1, authority.RoleHolders{}), nil},
		{"super re-levels to 1", aSuper.MayEditRole(mgrR, 1, authority.RoleHolders{}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestOverridesAndRolePermissions_PolicyComputesTheDelta: I3 — callers hand over
// only the new state; the "before" comes from the member MemberOf loaded, and
// nothing the policy trusts is computed by the caller.
func TestOverridesAndRolePermissions_PolicyComputesTheDelta(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ta := p.Meta.TenantAdmin
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, ta)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	routerR := mkRole(6, tA, "Lead Router", false, 40, false, p.Crm.Leads.Assign, p.Crm.Leads.Read)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	user := mkMember(t, 18, tA, true, "", userR)
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)
	aMgr := actorOf(t, mgr, authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll)
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	mgrHoldingTA := mkMember(t, 16, tA, true, "", mgrR)
	mgrGrantedTA := mkMemberOv(t, 19, tA, []membership.GrantedOverride{permanent(ta)}, nil, mgrR)
	mgrRevokedTA := mkMemberOv(t, 20, tA, nil, []string{ta}, mgrR)
	userGrantedAssign := mkMemberOv(t, 21, tA, []membership.GrantedOverride{permanent(p.Crm.Leads.Assign)}, nil, userR)
	userGrantedRead := mkMemberOv(t, 22, tA, []membership.GrantedOverride{permanent(p.Crm.Leads.Read)}, nil, userR)

	cases := []decision{
		// overrides: the ceiling is over what the replace adds or removes, measured
		// against the member's own snapshot
		{"admin strips tenant.admin from a member who holds it (granted)", aAdmin.MayChangeOverrides(mgrGrantedTA, authority.Overrides{}), authority.ErrGrantExceedsAuthority},
		{"admin lifts a tenant.admin revocation", aAdmin.MayChangeOverrides(mgrRevokedTA, authority.Overrides{}), authority.ErrGrantExceedsAuthority},
		{"admin adds a tenant.admin revocation", aAdmin.MayChangeOverrides(mgrHoldingTA, authority.Overrides{Revoked: []string{ta}}), authority.ErrGrantExceedsAuthority},
		{"owner strips tenant.admin from a manager", aOwner.MayChangeOverrides(mgrGrantedTA, authority.Overrides{}), nil},
		{"unchanged tenant.admin on both sides is no delta", aAdmin.MayChangeOverrides(mgrGrantedTA, authority.Overrides{Granted: grants(ta)}), nil},
		{"unchanged unheld grant is no delta", aMgr.MayChangeOverrides(userGrantedAssign, authority.Overrides{Granted: grants(p.Crm.Leads.Assign, p.Crm.Leads.Read)}), nil},
		{"adding an unheld grant", aMgr.MayChangeOverrides(userGrantedRead, authority.Overrides{Granted: grants(p.Crm.Leads.Read, p.Crm.Leads.Assign)}), authority.ErrGrantExceedsAuthority},
		{"removing an unheld grant", aMgr.MayChangeOverrides(userGrantedAssign, authority.Overrides{}), authority.ErrGrantExceedsAuthority},
		{"revoking an unheld permission", aMgr.MayChangeOverrides(user, authority.Overrides{Revoked: []string{p.Crm.Leads.Assign}}), authority.ErrGrantExceedsAuthority},
		{"owner may not end with tenant.admin revoked", aOwner.MayChangeOverrides(owner2, authority.Overrides{Revoked: []string{ta}}), authority.ErrOwnerProtected},
		{"the owner guard binds a super user", aSuper.MayChangeOverrides(owner2, authority.Overrides{Revoked: []string{ta}}), authority.ErrOwnerProtected},
		{"an owner already revoked stays refused", aOwner.MayChangeOverrides(mkMemberOv(t, 17, tA, nil, []string{ta}, ownerR), authority.Overrides{Revoked: []string{ta}}), authority.ErrOwnerProtected},

		// role permissions: delta of the stored set against the new set
		{"replace with the identical set (unheld names already on the role)", aMgr.MayReplaceRolePermissions(routerR, []string{p.Crm.Leads.Read, p.Crm.Leads.Assign}, authority.RoleHolders{}), nil},
		{"replace dropping an unheld name", aMgr.MayReplaceRolePermissions(routerR, []string{p.Crm.Leads.Read}, authority.RoleHolders{}), authority.ErrGrantExceedsAuthority},
		{"replace adding a held name", aMgr.MayReplaceRolePermissions(routerR, []string{p.Crm.Leads.Read, p.Crm.Leads.Assign, p.Crm.Leads.ReadAll}, authority.RoleHolders{}), nil},
		{"replace adding tenant.admin as admin", aAdmin.MayReplaceRolePermissions(routerR, []string{ta}, authority.RoleHolders{}), authority.ErrGrantExceedsAuthority},
		{"grant a name the role holds (no change)", aMgr.MayGrantRolePermission(routerR, p.Crm.Leads.Assign, authority.RoleHolders{}), nil},
		{"grant an unheld name", aMgr.MayGrantRolePermission(routerR, p.Crm.Leads.Manage, authority.RoleHolders{}), authority.ErrGrantExceedsAuthority},
		{"grant a held name", aMgr.MayGrantRolePermission(routerR, p.Crm.Leads.ReadAll, authority.RoleHolders{}), nil},
		{"grant an unknown name", aSuper.MayGrantRolePermission(routerR, "nope", authority.RoleHolders{}), authority.ErrGrantExceedsAuthority},
		{"revoke a name the role lacks (no change)", aMgr.MayRevokeRolePermission(routerR, p.Crm.Leads.Manage, authority.RoleHolders{}), nil},
		{"revoke an unheld name the role holds", aMgr.MayRevokeRolePermission(routerR, p.Crm.Leads.Assign, authority.RoleHolders{}), authority.ErrGrantExceedsAuthority},
		{"revoke a held name", aMgr.MayRevokeRolePermission(routerR, p.Crm.Leads.Read, authority.RoleHolders{}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
	if got := authority.PermissionNames(routerR.Permissions()); len(got) != 2 {
		t.Errorf("policy mutated the role's permission names: %v", got)
	}
}

// TestSystemDefaultRolesAreImmutableForEveryActor: I5.
func TestSystemDefaultRolesAreImmutableForEveryActor(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ta := p.Meta.TenantAdmin
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, ta)
	mgrR := mkRole(3, tA, "Manager", false, 20, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	sysChild := mkRole(7, tA, "System Child", true, 40, false)

	var cases []decision
	for name, a := range map[string]authority.Actor{"owner": aOwner, "super": aSuper, "operator": aOp} {
		cases = append(cases,
			decision{name + " renames", a.MayEditRole(ownerR, authority.NoLevelChange, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " re-levels", a.MayEditRole(ownerR, 5, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " replaces permissions", a.MayReplaceRolePermissions(ownerR, nil, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " grants", a.MayGrantRolePermission(ownerR, p.Crm.Leads.Read, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " revokes tenant.admin", a.MayRevokeRolePermission(ownerR, ta, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " deletes", a.MayDeleteRole(ownerR, authority.RoleHolders{}), role.ErrSystemDefault},
			decision{name + " sets the parent of a system role", a.MaySetRoleParent(sysChild, mgrR), role.ErrSystemDefault},
		)
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestApproval_AdminTierApprovesEverythingRequestable: the owner's ruling —
// "if the manager is not there, admin can see through everything and approve everything".
func TestApproval_AdminTierApprovesEverythingRequestable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false) // no tenant.admin: an existing tenant's Administrator
	seniorAdminR := mkRole(9, tA, "Senior Admin", false, 5, false)
	almostR := mkRole(8, tA, "Almost Manager", false, 11, false)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read, p.Roles.Approve)
	tlR := mkRole(4, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Crm.Leads.Read, p.Roles.Approve)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)

	owner := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	admin2 := mkMember(t, 14, tA, true, "", adminR)
	seniorAdmin := mkMember(t, 20, tA, true, "", seniorAdminR)
	almost := mkMember(t, 21, tA, true, "", almostR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	otherMgr := mkMember(t, 16, tA, true, "", mgrR)
	tl := mkMember(t, 17, tA, true, mgr.ID(), tlR)
	user := mkMember(t, 18, tA, true, tl.ID(), userR)
	stranger := mkMember(t, 19, tA, true, otherMgr.ID(), userR)

	aOwner := actorOf(t, owner, authority.RankOwner, p.Meta.TenantAdmin)
	aAdminNoTA := actorOf(t, admin, authority.RankAdmin) // holds nothing: no tenant.admin, no crm
	aSeniorAdmin := actorOf(t, seniorAdmin, authority.Rank(5))
	aAlmost := actorOf(t, almost, authority.Rank(11), p.Roles.Approve, p.Crm.Leads.Read, p.Crm.Leads.Assign)
	aMgr := actorOf(t, mgr, authority.RankManager, p.Roles.Approve, p.Crm.Leads.Read)
	aMgrNoApprove := actorOf(t, mgr, authority.RankManager, p.Crm.Leads.Read)
	aTL := actorOf(t, tl, authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	aImpAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), IsImpersonation: true})

	cases := []decision{
		{"admin without tenant.admin approves a user's request", aAdminNoTA.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"admin without tenant.admin denies a user's request", aAdminNoTA.MayDeny(user), nil},
		{"admin approves a stranger's request (no manager link)", aAdminNoTA.MayApprove(stranger, p.Crm.Leads.Assign), nil},
		{"admin approves a manager's request", aAdminNoTA.MayApprove(mgr, p.Crm.Leads.Assign), nil},
		{"admin approves a peer admin", aAdminNoTA.MayApprove(admin2, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"admin denies a peer admin", aAdminNoTA.MayDeny(admin2), authority.ErrInsufficientRank},
		{"admin approves an owner", aAdminNoTA.MayApprove(owner, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"owner approves an admin", aOwner.MayApprove(admin, p.Crm.Leads.Assign), nil},
		{"owner approves another owner", aOwner.MayApprove(owner2, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"owner denies another owner", aOwner.MayDeny(owner2), authority.ErrInsufficientRank},
		{"admin-tier custom role (rank 5) approves", aSeniorAdmin.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"rank 11 is manager tier: not a reporting line, no approval", aAlmost.MayApprove(stranger, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"admin may not approve tenant.admin", aAdminNoTA.MayApprove(user, p.Meta.TenantAdmin), authority.ErrGrantExceedsAuthority},
		{"admin may not approve a platform-reserved name", aAdminNoTA.MayApprove(user, p.Platform.TenantsManage), authority.ErrGrantExceedsAuthority},
		{"admin may not approve an unknown name", aAdminNoTA.MayApprove(user, "nope"), authority.ErrGrantExceedsAuthority},
		{"an impersonating admin never approves", aImpAdmin.MayApprove(user, p.Crm.Leads.Read), authority.ErrImpersonationNotAllowed},
		{"an admin never approves their own request", aAdminNoTA.MayApprove(admin, p.Crm.Leads.Read), authority.ErrSelfTarget},

		// manager and team lead path unchanged
		{"manager who is not the requester's manager", aMgr.MayApprove(stranger, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"manager approves a report's request for a held permission", aMgr.MayApprove(tl, p.Crm.Leads.Read), nil},
		{"manager for a permission it does not hold", aMgr.MayApprove(tl, p.Crm.Leads.Assign), authority.ErrGrantExceedsAuthority},
		{"manager without roles.approve", aMgrNoApprove.MayApprove(tl, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"team lead for its own report, held permission", aTL.MayApprove(user, p.Crm.Leads.Read), nil},
		{"team lead for its own report, permission not held", aTL.MayApprove(user, p.Crm.Leads.Assign), authority.ErrGrantExceedsAuthority},
		{"team lead denies its own report", aTL.MayDeny(user), nil},
		{"team lead for someone else's report", aTL.MayApprove(stranger, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"team lead for their own manager", aTL.MayApprove(mgr, p.Crm.Leads.Read), authority.ErrInsufficientRank},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestManagerHierarchy_PeerManagerCannotTakeOverAReport: strict hierarchy — to
// replace or clear someone's manager the actor is that manager or outranks it.
func TestManagerHierarchy_PeerManagerCannotTakeOverAReport(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgrA := mkMember(t, 31, tA, true, "", mgrR)
	mgrB := mkMember(t, 32, tA, true, "", mgrR)
	report := mkMember(t, 33, tA, true, mgrB.ID(), userR)
	aMgrA := actorOf(t, mgrA, authority.RankManager)
	aMgrB := actorOf(t, mgrB, authority.RankManager)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)

	cases := []decision{
		{"peer manager takes the report (replace)", aMgrA.MayReplaceManager(report, mgrA, &mgrB), authority.ErrInsufficientRank},
		{"peer manager clears the report's manager", aMgrA.MayClearManager(report, &mgrB), authority.ErrInsufficientRank},
		{"the manager gives the report to a more senior manager", aMgrB.MayReplaceManager(report, mgrB, &mgrB), nil},
		{"the manager clears its own report", aMgrB.MayClearManager(report, &mgrB), nil},
		{"an admin strictly outranks the manager", aAdmin.MayReplaceManager(report, mgrA, &mgrB), nil},
		{"an admin clears a manager's report", aAdmin.MayClearManager(report, &mgrB), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestDeletedRoles: M3.
func TestDeletedRoles(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	goneLow := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(40)), TenantID: tA, Name: "Gone", HierarchyLevel: 60, IsDeleted: true, CreatedAt: t0,
		Permissions: []*permission.Permission{permission.FromConstant(p.Meta.TenantAdmin)}})
	goneHigh := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(41)), TenantID: tA, Name: "Gone Senior", HierarchyLevel: 5, IsDeleted: true, CreatedAt: t0})
	goneParent := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(42)), TenantID: tA, Name: "Gone Parent", HierarchyLevel: 5, IsDeleted: true, CreatedAt: t0})

	admin := mkMember(t, 13, tA, true, "", adminR)
	holder := mkMember(t, 18, tA, true, "", userR, goneLow, goneHigh)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)

	cases := []decision{
		{"assigning a deleted role", aAdmin.MayAssignRole(holder, goneLow), role.ErrDeleted},
		{"revoking a deleted role needs rank only, not the ceiling (it carries tenant.admin)", aAdmin.MayRevokeRole(holder, goneLow, nil), nil},
		{"revoking a deleted role above the actor's rank is still refused on rank", aAdmin.MayRevokeRole(holder, goneHigh, nil), authority.ErrInsufficientRank},
		{"revoking a deleted role from a peer refused on rank", aAdmin.MayRevokeRole(mkMember(t, 19, tA, true, "", adminR), goneLow, nil), authority.ErrInsufficientRank},
		{"a deleted parent on set", aAdmin.MaySetRoleParent(userR, goneParent), authority.ErrRoleParentNotEligible},
		{"a deleted parent on create", aAdmin.MayCreateRole(tA, 50, nil, goneParent), authority.ErrRoleParentNotEligible},
		{"a live parent on set", aAdmin.MaySetRoleParent(userR, mgrR), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestMayCreateRole_Parent: M7 — create-with-parent is one decision.
func TestMayCreateRole_Parent(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	legacyZero := mkRole(9, tA, "Legacy Zero", false, 0, false) // custom role stored at level 0: it ranks as the user tier (50)
	foreignMgr := mkRole(10, tB, "Foreign Manager", false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)

	cases := []decision{
		{"parent more senior than the new level", aOwner.MayCreateRole(tA, 30, nil, mgrR), nil},
		{"parent at the same level", aOwner.MayCreateRole(tA, 20, nil, mgrR), authority.ErrRoleParentNotEligible},
		{"parent more junior", aOwner.MayCreateRole(tA, 10, nil, mgrR), authority.ErrRoleParentNotEligible},
		{"parent from another tenant", aOwner.MayCreateRole(tA, 30, nil, foreignMgr), authority.ErrNoAuthority},
		{"no parent", aOwner.MayCreateRole(tA, 30, nil, nil), nil},
		{"a level-0 custom parent ranks as 50: a level-2 child is not below it", aOwner.MayCreateRole(tA, 2, nil, legacyZero), authority.ErrRoleParentNotEligible},
		{"a level-0 custom parent with a level-50 child", aOwner.MayCreateRole(tA, 50, nil, legacyZero), authority.ErrRoleParentNotEligible},
		{"a level-0 custom parent with a level-51 child", aOwner.MayCreateRole(tA, 51, nil, legacyZero), nil},
		{"set: a level-0 custom parent with a level-1 child", aOwner.MaySetRoleParent(mkRole(11, tA, "Level One", false, 1, false), legacyZero), authority.ErrRoleParentNotEligible},
		{"set: a level-0 custom parent with a user child", aOwner.MaySetRoleParent(userR, legacyZero), authority.ErrRoleParentNotEligible},
		{"set: a level-0 custom parent with a level-60 child", aOwner.MaySetRoleParent(mkRole(12, tA, "Level Sixty", false, 60, false), legacyZero), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestMemberOf_PersonActive: M4 — a member whose person cannot act is not active.
func TestMemberOf_PersonActive(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)

	suspendedOwner, err := authority.MemberOf(mkMembership(11, tA, true, "", ownerR), []*role.Role{ownerR}, t0, suspendedPerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	suspendedOwner2, err := authority.MemberOf(mkMembership(16, tA, true, "", ownerR), []*role.Role{ownerR}, t0, suspendedPerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	liveOwner := mkMember(t, 12, tA, true, "", ownerR)
	otherOwner := mkMember(t, 13, tA, true, "", ownerR)
	suspendedMgr, err := authority.MemberOf(mkMembership(14, tA, true, "", mgrR), []*role.Role{mgrR}, t0, suspendedPerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	liveMgr := mkMember(t, 15, tA, true, "", mgrR)
	user := mkMember(t, 18, tA, true, "", userR)
	admin := mkMember(t, 19, tA, true, "", adminR)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)

	if suspendedOwner.IsActive() || !liveOwner.IsActive() {
		t.Errorf("IsActive: suspended person %v, live person %v", suspendedOwner.IsActive(), liveOwner.IsActive())
	}
	cases := []decision{
		{"the only other owner's person is suspended: last owner", authority.GuardLastOwner(liveOwner, []authority.Member{liveOwner, suspendedOwner}), authority.ErrLastOwnerProtected},
		{"a live other owner remains", authority.GuardLastOwner(liveOwner, []authority.Member{liveOwner, suspendedOwner, otherOwner}), nil},
		{"a suspended sole owner is still the last owner", authority.GuardLastOwner(suspendedOwner, []authority.Member{suspendedOwner}), authority.ErrLastOwnerProtected},
		{"a suspended owner with a live other owner may go", authority.GuardLastOwner(suspendedOwner, []authority.Member{suspendedOwner, liveOwner}), nil},
		{"a suspended owner whose only other owner is suspended too is protected", authority.GuardLastOwner(suspendedOwner, []authority.Member{suspendedOwner, suspendedOwner2}), authority.ErrLastOwnerProtected},
		{"a manager whose person is suspended is not eligible", aAdmin.MayReplaceManager(user, suspendedMgr, nil), authority.ErrManagerNotEligible},
		{"a live manager is eligible", aAdmin.MayReplaceManager(user, liveMgr, nil), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestSameTenantIsCanonical: a role or member whose tenant is spelled differently
// is the same tenant; an operator is NOT exempt inside its own tenant.
func TestSameTenantIsCanonical(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	upperOwn := mkRole(41, tenant.ID(strings.ToUpper(tP.String())), "Own Manager", false, 20, false)
	braced := mkRole(42, tenant.ID("{"+tP.String()+"}"), "Own Braced", false, 20, false)
	other := mkRole(43, tB, "Other Manager", false, 20, false)

	cases := []decision{
		{"operator edits a role of its own tenant spelled in upper case: follows rank", op.MayEditRole(upperOwn, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"operator edits a role of its own tenant spelled with braces", op.MayEditRole(braced, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"operator deletes a role of its own tenant in upper case", op.MayDeleteRole(upperOwn, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"operator edits another tenant's role", op.MayEditRole(other, authority.NoLevelChange, authority.RoleHolders{}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestNilRoleNeverPanics: every decision that takes a role answers no authority for nil.
func TestNilRoleNeverPanics(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	admin := mkMember(t, 13, tA, true, "", adminR)
	user := mkMember(t, 18, tA, true, "", userR)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)

	cases := []decision{
		{"assign nil", aAdmin.MayAssignRole(user, nil), authority.ErrNoAuthority},
		{"revoke nil", aAdmin.MayRevokeRole(user, nil, nil), authority.ErrNoAuthority},
		{"edit nil", aAdmin.MayEditRole(nil, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrNoAuthority},
		{"replace permissions of nil", aAdmin.MayReplaceRolePermissions(nil, nil, authority.RoleHolders{}), authority.ErrNoAuthority},
		{"grant on nil", aAdmin.MayGrantRolePermission(nil, "crm.leads.read", authority.RoleHolders{}), authority.ErrNoAuthority},
		{"revoke on nil", aAdmin.MayRevokeRolePermission(nil, "crm.leads.read", authority.RoleHolders{}), authority.ErrNoAuthority},
		{"delete nil", aAdmin.MayDeleteRole(nil, authority.RoleHolders{}), authority.ErrNoAuthority},
		{"set parent of nil child", aAdmin.MaySetRoleParent(nil, userR), authority.ErrNoAuthority},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestLegacyLevelZeroCustomRole: a custom role stored at level 0 ranks as the
// user tier (fail closed); an owner repairs it, and an admin outranks it.
func TestLegacyLevelZeroCustomRole(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	legacy := mkRole(9, tA, "Legacy Zero", false, 0, false, p.Crm.Leads.Read)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	holder := mkMember(t, 14, tA, true, "", legacy)
	aOwner := actorOf(t, owner, authority.RankOwner, adminEffective()...)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)
	userR := mkRole(3, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	user := mkMember(t, 15, tA, true, "", userR)
	aUser := actorOf(t, user, authority.RankUser, adminEffective()...)

	cases := []decision{
		{"owner renames a legacy level-0 role", aOwner.MayEditRole(legacy, authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"owner re-levels a legacy level-0 role into the ladder", aOwner.MayEditRole(legacy, 5, authority.RoleHolders{}), nil},
		{"owner deletes a legacy level-0 role", aOwner.MayDeleteRole(legacy, authority.RoleHolders{}), nil},
		{"admin edits a legacy level-0 role (it ranks as the user tier)", aAdmin.MayEditRole(legacy, authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"a user-tier actor may not touch it", aUser.MayEditRole(legacy, authority.NoLevelChange, authority.RoleHolders{}), authority.ErrInsufficientRank},
		{"admin may not assign it", aAdmin.MayAssignRole(admin, legacy), authority.ErrSelfTarget},
		{"owner assigns it", aOwner.MayAssignRole(admin, legacy), nil},
		{"admin acts on its holder (ranks as the user tier)", aAdmin.MayActOn(holder), nil},
		{"a user-tier actor may not act on its holder", aUser.MayActOn(holder), authority.ErrInsufficientRank},
		{"owner acts on its holder", aOwner.MayActOn(holder), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
	if holder.Rank() != authority.RankUser || holder.IsOwner() {
		t.Errorf("legacy level-0 holder: rank %d owner %v, want rank %d and not an owner", holder.Rank(), holder.IsOwner(), authority.RankUser)
	}
}

// TestOutOfRangeRoleLevelsRankNone: a negative or oversized stored level is no rank at all.
func TestOutOfRangeRoleLevelsRankNone(t *testing.T) {
	t.Parallel()
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, permission.IdentityPermissions.Meta.TenantAdmin)
	negative := mkRole(20, tA, "Negative", false, -5, false)
	huge := mkRole(21, tA, "Huge", false, 150, false)
	admin := mkMember(t, 13, tA, true, "", adminR)
	holder := mkMember(t, 14, tA, true, "", negative)
	aAdmin := actorOf(t, admin, authority.RankAdmin, adminEffective()...)

	if got := authority.RankOf([]*role.Role{negative}); got != authority.RankNone {
		t.Errorf("RankOf(level -5) = %d, want RankNone", got)
	}
	if got := authority.RankOf([]*role.Role{huge, negative}); got != authority.RankNone {
		t.Errorf("RankOf(level 150, -5) = %d, want RankNone", got)
	}
	if holder.Rank() != authority.RankNone {
		t.Errorf("holder of a negative-level role ranks %d, want RankNone", holder.Rank())
	}
	cases := []decision{
		{"admin edits a negative-level role (it is the weakest)", aAdmin.MayEditRole(negative, authority.NoLevelChange, authority.RoleHolders{}), nil},
		{"admin assigns a negative-level role", aAdmin.MayAssignRole(holder, negative), nil},
		{"a negative-level role is not a valid parent", aAdmin.MaySetRoleParent(huge, negative), authority.ErrRoleParentNotEligible},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestSuperAdminSystemRoleHolderIsNotAnOwner: a system-default super-admin role
// at level 0 is not CompanyOwner.
func TestSuperAdminSystemRoleHolderIsNotAnOwner(t *testing.T) {
	t.Parallel()
	superR := mkRole(7, tA, "SuperAdmin", true, 0, true)
	holder := mkMember(t, 23, tA, true, "", superR)
	if holder.IsOwner() || !holder.IsSuperAdmin() {
		t.Errorf("super-admin holder: owner=%v superAdmin=%v, want a super admin who is not an owner", holder.IsOwner(), holder.IsSuperAdmin())
	}
	if holder.Rank() != authority.RankUser {
		t.Errorf("super-admin holder at level 0 ranks %d, want %d (level 0 counts only on the CompanyOwner role)", holder.Rank(), authority.RankUser)
	}
	// Bootstrap and the data migration store the SuperAdmin role at level 1.
	migrated := mkMember(t, 24, tA, true, "", mkRole(8, tA, "SuperAdmin", true, 1, true))
	if migrated.Rank() != 1 {
		t.Errorf("super-admin holder at level 1 ranks %d, want 1", migrated.Rank())
	}
	if err := authority.GuardLastOwner(holder, nil); err != nil {
		t.Errorf("a non-owner target is never the last owner: %v", err)
	}
}

// TestNewActor_TenantAdminDoesNotExpandToPlatformNames: holding tenant.admin
// must not make an operator "exempt" (platform.users.manage is reserved).
func TestNewActor_TenantAdminDoesNotExpandToPlatformNames(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	userR := mkRole(25, tB, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	foreign := mkMember(t, 22, tB, true, "", userR)
	withAdmin := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Meta.TenantAdmin}})
	withManage := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	cases := []decision{
		{"operator holding only tenant.admin is not exempt in another tenant", withAdmin.MayActOn(foreign), authority.ErrNoAuthority},
		{"operator holding platform.users.manage is exempt", withManage.MayActOn(foreign), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestMemberOf_MalformedIdsNeverMatchEachOther: two unparseable ids ("" and "")
// are not "the same role".
func TestMemberOf_MalformedIdsNeverMatchEachOther(t *testing.T) {
	t.Parallel()
	for name, id := range map[string]string{"empty": "", "garbage": "garbage", "nil uuid": "00000000-0000-0000-0000-000000000000"} {
		r := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(id), TenantID: tA, Name: "Odd", HierarchyLevel: 50, CreatedAt: t0})
		both := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(11)), PersonID: person.ID(uid(511)), TenantID: tA, Status: membership.StatusActive, JoinedAt: t0, RoleAssignments: []role.ID{role.ID(id)}})
		if got, err := authority.MemberOf(both, []*role.Role{r}, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) || got.Valid() {
			t.Errorf("%s on both sides: (%+v, %v), want invalid and no authority", name, got, err)
		}
		assignedOnly := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(12)), PersonID: person.ID(uid(512)), TenantID: tA, Status: membership.StatusActive, JoinedAt: t0, RoleAssignments: []role.ID{role.ID(id)}})
		good := mkRole(5, tA, "Good", false, 50, false)
		if _, err := authority.MemberOf(assignedOnly, []*role.Role{good}, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("%s assigned, a different role loaded: %v, want no authority", name, err)
		}
		loadedOnly := mkMembership(13, tA, true, "", good)
		if _, err := authority.MemberOf(loadedOnly, []*role.Role{good, r}, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("%s loaded, not assigned: %v, want no authority", name, err)
		}
	}
}
