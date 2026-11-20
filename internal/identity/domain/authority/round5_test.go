package authority_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// r5World is the cast of the hierarchy round 5 rules: a custom level-30 role C
// that several kinds of member hold, and the actors who edit it.
type r5World struct {
	c                        *role.Role // the role being edited, level 30
	owner, admin, mgr        authority.Actor
	super, op                authority.Actor
	peerMgr, tlHolder, exec  authority.Member // holders of C (peerMgr holds Sales Manager too)
	inactiveAdmin            authority.Member // an inactive admin holding C
	ownerHolder, superHolder authority.Member
	mgrHolder                authority.Member // the acting manager's own membership, holding C
	ownerM                   authority.Member
}

func newR5World(t *testing.T) r5World {
	t.Helper()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false, p.Crm.Leads.Read)
	superR := mkRole(7, tA, "SuperAdmin", true, 0, true)
	c := mkRole(30, tA, "Shared custom", false, 30, false, p.Crm.Leads.Read)

	w := r5World{c: c}
	w.ownerM = mkMember(t, 11, tA, true, "", ownerR, c)
	mgrM := mkMember(t, 15, tA, true, "", mgrR)
	w.mgrHolder = mkMember(t, 15, tA, true, "", mgrR, c)
	w.peerMgr = mkMember(t, 16, tA, true, "", mgrR, c)
	w.tlHolder = mkMember(t, 17, tA, true, "", c)
	w.exec = mkMember(t, 18, tA, true, "", userR, c)
	w.inactiveAdmin = mkMember(t, 20, tA, false, "", adminR, c)
	w.ownerHolder = mkMember(t, 12, tA, true, "", ownerR, c)
	w.superHolder = mkMember(t, 23, tA, true, "", superR, c)

	w.owner = actorOf(t, mkMember(t, 11, tA, true, "", ownerR), authority.RankOwner, adminEffective()...)
	w.admin = actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	w.mgr = actorOf(t, mgrM, authority.RankManager, p.Crm.Leads.Read, p.Roles.Update)
	w.super = mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	w.op = mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.RolesManage}})
	return w
}

func hs(ms ...authority.Member) authority.RoleHolders { return authority.RoleHolders{Holders: ms} }

// TestRoleEdits_ActOnEveryActiveHolder is the holders rule: every role decision
// that changes what a role confers (and a re-level, and a delete) is a decision
// to act on each active holder.
func TestRoleEdits_ActOnEveryActiveHolder(t *testing.T) {
	t.Parallel()
	w := newR5World(t)
	perm := permission.IdentityPermissions.Crm.Leads.Read // held by every actor that has to hold the name
	// each role decision, run with the same holders
	run := func(a authority.Actor, h authority.RoleHolders) map[string]error {
		return map[string]error{
			"grant":   a.MayGrantRolePermission(w.c, perm, h),
			"revoke":  a.MayRevokeRolePermission(w.c, perm, h),
			"replace": a.MayReplaceRolePermissions(w.c, nil, h),
			"delete":  a.MayDeleteRole(w.c, h),
			"relevel": a.MayEditRole(w.c, 40, h),
		}
	}
	cases := []struct {
		name string
		a    authority.Actor
		h    authority.RoleHolders
		want error
	}{
		{"manager, a peer manager holds it", w.mgr, hs(w.peerMgr), authority.ErrInsufficientRank},
		{"manager, a junior executive holds it", w.mgr, hs(w.exec), nil},
		{"manager, a junior and a peer hold it (one bad holder refuses all)", w.mgr, hs(w.exec, w.peerMgr), authority.ErrInsufficientRank},
		{"manager, an active senior admin holds it", w.mgr, authority.RoleHolders{Holders: []authority.Member{mkMember(t, 21, tA, true, "", mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false), w.c)}}, authority.ErrInsufficientRank},
		{"manager, the only senior holder is inactive", w.mgr, hs(w.exec, w.inactiveAdmin), nil},
		{"manager holds the role themself", w.mgr, hs(w.mgrHolder, w.exec), authority.ErrSelfTarget},
		{"owner holds the role themself", w.owner, hs(w.ownerM, w.exec), authority.ErrSelfTarget},
		{"owner, another owner holds it", w.owner, hs(w.ownerHolder, w.exec), nil},
		{"admin, an owner holds it", w.admin, hs(w.ownerHolder), authority.ErrInsufficientRank},
		{"admin, a super-admin member holds it", w.admin, hs(w.superHolder), authority.ErrInsufficientRank},
		{"owner, a super-admin member holds it", w.owner, hs(w.superHolder), authority.ErrInsufficientRank},
		{"super user, any holders", w.super, hs(w.ownerHolder, w.peerMgr, w.superHolder, w.exec), nil},
		{"exempt operator elsewhere, any holders", w.op, hs(w.peerMgr, w.exec), nil},
		{"no holders at all", w.mgr, authority.RoleHolders{}, nil},
		{"a zero holder is no authority", w.mgr, hs(authority.Member{}), authority.ErrNoAuthority},
	}
	for _, c := range cases {
		for op, got := range run(c.a, c.h) {
			// only "who holds it" varies, so every decision gives the same verdict
			want := c.want
			if (want == nil) != (got == nil) || (want != nil && !errors.Is(got, want)) {
				t.Errorf("%s / %s: %v, want %v", c.name, op, got, want)
			}
		}
	}
}

// A rename needs no holders, and a re-level to the level the role already has
// changes no holder: neither consults them.
func TestRoleEdits_RenameAndNoOpReLevelIgnoreHolders(t *testing.T) {
	t.Parallel()
	w := newR5World(t)
	cases := []decision{
		{"rename only", w.mgr.MayEditRole(w.c, authority.NoLevelChange, hs(w.peerMgr)), nil},
		{"re-level to the current level", w.mgr.MayEditRole(w.c, 30, hs(w.peerMgr)), nil},
		{"a real re-level does", w.mgr.MayEditRole(w.c, 31, hs(w.peerMgr)), authority.ErrInsufficientRank},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
	if authority.ChangesLevel(w.c, authority.NoLevelChange) || authority.ChangesLevel(w.c, 30) || !authority.ChangesLevel(w.c, 31) || authority.ChangesLevel(nil, 31) {
		t.Error("ChangesLevel must be true only for a real move of a real role")
	}
}

// TestReLevel_KeepsReportingLinesStrictlyOrdered: the re-level reporting rule.
func TestReLevel_KeepsReportingLinesStrictlyOrdered(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	seniorR := mkRole(4, tA, "Senior", false, 15, false)
	execRole := mkRole(31, tA, "Exec2", false, 50, false) // the role being edited
	other := mkRole(32, tA, "Other", false, 25, false)    // another role a holder may hold
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	admin := actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})

	mgrM := mkMember(t, 40, tA, true, "", mgrR)
	seniorM := mkMember(t, 41, tA, true, "", seniorR)
	underMgr := mkMember(t, 42, tA, true, mgrM.ID(), execRole)             // holder reporting to a rank-20 manager
	underSenior := mkMember(t, 43, tA, true, seniorM.ID(), execRole)       // holder reporting to a rank-15 manager
	twoRoles := mkMember(t, 44, tA, true, mgrM.ID(), execRole, other)      // holder whose other role is rank 25
	inactive := mkMember(t, 45, tA, false, mgrM.ID(), execRole)            // inactive holder under the manager
	orphan := mkMember(t, 46, tA, true, membership.ID(uid(999)), execRole) // manager never loaded
	boss := mkMember(t, 47, tA, true, "", execRole)                        // a holder with a direct report
	report := mkMember(t, 48, tA, true, boss.ID(), mkRole(33, tA, "Report", false, 60, false))
	bossAndReport := mkMember(t, 49, tA, true, boss.ID(), execRole) // both boss and report hold the role

	linked := func(ms ...authority.Member) []authority.Member { return ms }
	h := func(holders []authority.Member, link ...authority.Member) authority.RoleHolders {
		return authority.RoleHolders{Holders: holders, Linked: link}
	}
	cases := []decision{
		// the holder against their manager
		{"new level equal to the manager's rank breaks the line", admin.MayEditRole(execRole, 20, h(linked(underMgr), mgrM)), authority.ErrRoleLevelBreaksReporting},
		{"new level senior to the manager's rank breaks the line", admin.MayEditRole(execRole, 16, h(linked(underMgr), mgrM)), authority.ErrRoleLevelBreaksReporting},
		{"new level one below the manager's rank holds", admin.MayEditRole(execRole, 21, h(linked(underMgr), mgrM)), nil},
		{"a senior manager is not broken by the same move", admin.MayEditRole(execRole, 16, h(linked(underSenior), seniorM)), nil},
		{"the holder's other role keeps their rank (25 stays junior to 20)", admin.MayEditRole(execRole, 30, h(linked(twoRoles), mgrM)), nil},
		{"the holder's other role decides when it is more senior than the new level", admin.MayEditRole(execRole, 90, h(linked(twoRoles), mgrM)), nil},
		{"an inactive holder is not checked", admin.MayEditRole(execRole, 11, h(linked(inactive), mgrM)), nil},
		{"a manager that was not loaded fails closed", admin.MayEditRole(execRole, 90, h(linked(orphan))), authority.ErrNoAuthority},
		// the holder against their direct reports
		{"a holder moved to or below their report breaks the line (the report is at 60)", admin.MayEditRole(execRole, 60, h(linked(boss), report)), authority.ErrRoleLevelBreaksReporting},
		{"a holder moved just above their report holds", admin.MayEditRole(execRole, 59, h(linked(boss), report)), nil},
		{"both ends hold the role: both move to the same level", admin.MayEditRole(execRole, 40, h(linked(boss, bossAndReport))), authority.ErrRoleLevelBreaksReporting},
		// a re-level that moves nobody's rank
		{"a no-op re-level is not checked", admin.MayEditRole(execRole, 50, h(linked(underMgr), mgrM)), nil},
		{"the rule binds a super user too", super.MayEditRole(execRole, 20, h(linked(underMgr), mgrM)), authority.ErrRoleLevelBreaksReporting},
		{"no holders, nothing to break", admin.MayEditRole(execRole, 20, authority.RoleHolders{}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestUnrevoke_NeedsTheAdminTier: removing a name from a member's revoked list
// needs rank <= Admin (or exemption), on top of the ceiling.
func TestUnrevoke_NeedsTheAdminTier(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	target := mkMemberOv(t, 18, tA, nil, []string{x}, userR)
	targetOther := mkMemberOv(t, 19, tA, nil, []string{p.Crm.Leads.Read}, userR)

	aOwner := actorOf(t, mkMember(t, 11, tA, true, "", ownerR), authority.RankOwner, adminEffective()...)
	aAdmin := actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	aMgr := actorOf(t, mkMember(t, 15, tA, true, "", mgrR), authority.RankManager, x, p.Crm.Leads.Read)
	aSenior := actorOf(t, mkMember(t, 16, tA, true, "", mkRole(8, tA, "Senior", false, 15, false)), authority.RankAdmin+5, x)
	aAdminNoName := actorOf(t, mkMember(t, 17, tA, true, "", adminR), authority.RankAdmin, p.Crm.Leads.Read)
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	imp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, Rank: rk(authority.RankOwner), IsImpersonation: true, EffectivePermissions: adminEffective()})

	lift := authority.Overrides{}
	keep := authority.Overrides{Revoked: []string{x}}
	addMore := authority.Overrides{Revoked: []string{x, p.Crm.Leads.Read}}
	cases := []decision{
		{"manager holding the name lifts the revoke", aMgr.MayChangeOverrides(target, lift), authority.ErrGrantExceedsAuthority},
		{"a rank-15 senior manager is still below the Admin tier", aSenior.MayChangeOverrides(target, lift), authority.ErrGrantExceedsAuthority},
		{"manager leaves the revoke in place", aMgr.MayChangeOverrides(target, keep), nil},
		{"manager adds a revoke without lifting one", aMgr.MayChangeOverrides(target, addMore), nil},
		{"manager lifts one revoke of two, keeping the other", aMgr.MayChangeOverrides(mkMemberOv(t, 20, tA, nil, []string{x, p.Crm.Leads.Read}, userR), authority.Overrides{Revoked: []string{p.Crm.Leads.Read}}), authority.ErrGrantExceedsAuthority},
		{"admin holding the name lifts it", aAdmin.MayChangeOverrides(target, lift), nil},
		{"admin not holding the name still hits the ceiling", aAdminNoName.MayChangeOverrides(target, lift), authority.ErrGrantExceedsAuthority},
		{"owner lifts it", aOwner.MayChangeOverrides(target, lift), nil},
		{"an impersonation session is Admin-equivalent", imp.MayChangeOverrides(target, lift), nil},
		{"super user lifts it", super.MayChangeOverrides(target, lift), nil},
		{"exempt operator lifts it", op.MayChangeOverrides(target, lift), nil},
		{"lifting a revoke the manager holds, on another name", aMgr.MayChangeOverrides(targetOther, lift), authority.ErrGrantExceedsAuthority},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestMayReactivate_OwnersFromOutsideNeedTheOwnershipPermission mirrors the
// deactivation gate.
func TestMayReactivate_OwnersFromOutsideNeedTheOwnershipPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner2 := mkMember(t, 12, tA, false, "", ownerR)
	user := mkMember(t, 18, tA, false, "", userR)
	mk := func(perms ...string) authority.Actor {
		return mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: perms})
	}
	aOwner := actorOf(t, mkMember(t, 11, tA, true, "", ownerR), authority.RankOwner, adminEffective()...)
	aAdmin := actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	cases := []decision{
		{"operator with users.manage only, an owner", mk(p.Platform.UsersManage).MayReactivate(owner2), authority.ErrOwnershipAuthorityRequired},
		{"operator with roles.manage only, an owner", mk(p.Platform.RolesManage).MayReactivate(owner2), authority.ErrOwnershipAuthorityRequired},
		{"operator with the ownership permission, an owner", mk(p.Platform.UsersManage, p.Platform.TenantsOwnership).MayReactivate(owner2), nil},
		{"operator with users.manage only, a non-owner", mk(p.Platform.UsersManage).MayReactivate(user), nil},
		{"super user, an owner", super.MayReactivate(owner2), nil},
		{"an owner inside the tenant, an owner", aOwner.MayReactivate(owner2), nil},
		{"an admin inside the tenant, an owner (rank decides)", aAdmin.MayReactivate(owner2), authority.ErrInsufficientRank},
		{"an admin, a non-owner", aAdmin.MayReactivate(user), nil},
		{"zero actor", authority.Actor{}.MayReactivate(user), authority.ErrNoAuthority},
		{"zero member", aOwner.MayReactivate(authority.Member{}), authority.ErrNoAuthority},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}
