package authority_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// r5World is the cast of the hierarchy round 5 rules: a custom level-30 role C
// that several kinds of member hold, and the actors who edit it.
type r5World struct {
	c                        *role.Role // the role being edited, level 30
	owner, admin, mgr        authority.Actor
	super, op                authority.Actor
	peerMgr, tlHolder, exec  authority.Member // holders of C (peerMgr holds Sales Manager too)
	inactiveAdmin            authority.Member // an inactive admin holding C
	inactiveExec             authority.Member // an inactive executive holding C
	inactivePeer             authority.Member // an inactive peer manager holding C
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
	w.inactiveExec = mkMember(t, 24, tA, false, "", userR, c)
	w.inactivePeer = mkMember(t, 25, tA, false, "", mgrR, c)
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
			"delete":  a.MayDeactivateRole(w.c, h),
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
		// hierarchy round 6: an inactive holder counts (fail closed)
		{"manager, the only senior holder is inactive", w.mgr, hs(w.exec, w.inactiveAdmin), authority.ErrInsufficientRank},
		{"admin, an inactive junior holder", w.admin, hs(w.exec, w.inactiveExec), nil},
		{"manager, an inactive peer holder", w.mgr, hs(w.exec, w.inactivePeer), authority.ErrInsufficientRank},
		{"manager holds the role themself", w.mgr, hs(w.mgrHolder, w.exec), authority.ErrSelfTarget},
		// hierarchy round 6: owners are exempt from the self-holder refusal
		{"owner holds the role themself", w.owner, hs(w.ownerM, w.exec), nil},
		{"owner holds the role themself and another owner does too", w.owner, hs(w.ownerM, w.ownerHolder), nil},
		{"admin holds the role themself", w.admin, hs(mkMember(t, 13, tA, true, "", mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false), w.c)), authority.ErrSelfTarget},
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

	highR := mkRole(37, tA, "High", false, 15, false)
	brokenUnder := mkMember(t, 50, tA, true, mgrM.ID(), highR) // rank 15 under a rank-20 manager: broken already
	inactiveMgr := mkMember(t, 51, tA, false, "", mkRole(35, tA, "Gone manager", false, 40, false))
	underInactiveMgr := mkMember(t, 52, tA, true, inactiveMgr.ID(), execRole)
	inactiveReport := mkMember(t, 53, tA, false, boss.ID(), mkRole(36, tA, "Gone report", false, 60, false))
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
		// hierarchy round 6: only a NEWLY broken line blocks
		{"both ends hold the role: they were equal already, so nothing is newly broken", admin.MayEditRole(execRole, 40, h(linked(boss, bossAndReport))), nil},
		{"a line already broken and still broken does not block", admin.MayEditRole(highR, 18, h(linked(brokenUnder), mgrM)), nil},
		{"a re-level that improves a broken line is allowed", admin.MayEditRole(highR, 25, h(linked(brokenUnder), mgrM)), nil},
		{"an inactive manager is ignored", admin.MayEditRole(execRole, 40, h(linked(underInactiveMgr), inactiveMgr)), nil},
		{"the same move against an active manager is refused", admin.MayEditRole(execRole, 20, h(linked(underMgr), mgrM)), authority.ErrRoleLevelBreaksReporting},
		{"an inactive direct report is ignored", admin.MayEditRole(execRole, 60, h(linked(boss), inactiveReport)), nil},
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

// TestApprove_AdminMustHoldList pins both sides of every name an Admin-tier
// approver may approve only while holding it (hierarchy round 5 added the last
// nine); a day-to-day name stays approvable without holding it.
func TestApprove_AdminMustHoldList(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	requester := mkMember(t, 18, tA, true, "", userR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mustHold := []string{
		p.Roles.Assign, p.Roles.Revoke, p.Roles.Approve,
		p.Users.UpdatePermissions,
		p.Tenants.Update, p.Tenants.UpdateSettings, p.PlatformMarketplace.Purchase, p.Orders.Payments.Record,
		p.Meta.RequestPermissionElevation, p.Users.View, p.Roles.View, p.Tenants.View,
		p.Crm.Leads.ReadAll, p.Tasks.WorkItems.ReadAll, p.Orders.Payments.Read, p.Orders.Invoices.Read, p.PlatformLeadCredits.Read,
	}
	var cases []decision
	bare := actorOf(t, admin, authority.RankAdmin, p.Crm.Leads.Read)
	for _, name := range mustHold {
		holder := actorOf(t, admin, authority.RankAdmin, name)
		cases = append(cases,
			decision{"an admin who holds " + name, holder.MayApprove(requester, name), nil},
			decision{"an admin who does not hold " + name, bare.MayApprove(requester, name), authority.ErrGrantExceedsAuthority},
		)
	}
	// Hierarchy round 6 (the owner's ruling: Admin approves everything day-to-day): the
	// user-lifecycle names left the list, so HR approvals are not owner-only.
	for _, name := range []string{p.Crm.Leads.Assign, p.Tasks.WorkItems.Reassign, p.Inventory.Stock.Read,
		p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock} {
		cases = append(cases, decision{"an admin approves the day-to-day " + name + " without holding it", bare.MayApprove(requester, name), nil})
	}
	// Owner ruling 2026-11-21: users.create is admin-tier-only, so no approver confers it below the tier.
	cases = append(cases, decision{"an admin approves identity.users.create for a rank-50 requester", bare.MayApprove(requester, p.Users.Create), authority.ErrAdminTierOnly})
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// TestCapApprovalExpiry: an approval never outlives the approver's own grant.
func TestCapApprovalExpiry(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Tasks.WorkItems.Reassign
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Roles.Approve)
	mgrRoleHoldsX := mkRole(4, tA, "Mgr with X", false, 20, false, p.Roles.Approve, x)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	req := mkMember(t, 18, tA, true, "", userR)
	soon, later := t0.Add(24*time.Hour), t0.Add(90*24*time.Hour)
	timed := func(name string, at time.Time) membership.GrantedOverride {
		return membership.GrantedOverride{Permission: permission.FromConstant(name), ExpiresAt: at}
	}
	narrow := func(n int, rank authority.Rank, roles []*role.Role, granted []membership.GrantedOverride, viaRoles []string, effective ...string) authority.Actor {
		fresh := mkMemberOv(t, n, tA, granted, nil, roles...)
		token := mkActor(t, authority.ActorInput{MembershipID: fresh.ID(), TenantID: tA, Rank: rk(rank), EffectivePermissions: effective})
		a := token.NarrowedBy(fresh, effective, viaRoles)
		if !a.Valid() {
			t.Fatal("narrowed actor invalid")
		}
		return a
	}
	mgrTimed := narrow(15, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timed(x, soon)}, []string{p.Roles.Approve}, p.Roles.Approve, x)
	mgrRole := narrow(16, authority.RankManager, []*role.Role{mgrRoleHoldsX}, nil, []string{p.Roles.Approve, x}, p.Roles.Approve, x)
	mgrRoleAndTimed := narrow(20, authority.RankManager, []*role.Role{mgrRoleHoldsX}, []membership.GrantedOverride{timed(x, soon)}, []string{p.Roles.Approve, x}, p.Roles.Approve, x)
	mgrBoth := narrow(17, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timed(x, soon), {Permission: permission.FromConstant(x)}}, []string{p.Roles.Approve}, p.Roles.Approve, x)
	adminTimedMustHold := narrow(13, authority.RankAdmin, []*role.Role{adminR}, []membership.GrantedOverride{timed(p.Users.UpdatePermissions, soon)}, nil, p.Users.UpdatePermissions)
	adminTimedDayToDay := narrow(14, authority.RankAdmin, []*role.Role{adminR}, []membership.GrantedOverride{timed(x, soon)}, nil, x)
	mgrTimedAdminKey := narrow(19, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timed(p.Meta.TenantAdmin, soon)}, []string{p.Roles.Approve}, p.Roles.Approve, p.Meta.TenantAdmin, x)
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	reqB := mkMember(t, 28, tB, true, "", mkRole(25, tB, role.SystemRoles.Tenant.SalesExecutive, false, 50, false))

	cases := []struct {
		name string
		got  time.Time
		want time.Time
	}{
		{"manager holding X only through a timed override is capped", mgrTimed.CapApprovalExpiry(req, x, later), soon},
		{"an approval shorter than the approver's grant is untouched", mgrTimed.CapApprovalExpiry(req, x, soon.Add(-time.Hour)), soon.Add(-time.Hour)},
		{"manager holding X through a role is not capped", mgrRole.CapApprovalExpiry(req, x, later), later},
		{"manager holding X through a role AND a timed override is not capped", mgrRoleAndTimed.CapApprovalExpiry(req, x, later), later},
		{"manager with a permanent grant besides the timed one is not capped", mgrBoth.CapApprovalExpiry(req, x, later), later},
		{"admin approving a must-hold name held only by a timed grant is capped", adminTimedMustHold.CapApprovalExpiry(req, p.Users.UpdatePermissions, later), soon},
		{"admin approving a day-to-day name needs no holding, so no cap", adminTimedDayToDay.CapApprovalExpiry(req, x, later), later},
		{"a timed tenant.admin grant caps every name it implies", mgrTimedAdminKey.CapApprovalExpiry(req, x, later), soon},
		{"super user is never capped", super.CapApprovalExpiry(reqB, x, later), later},
		{"zero actor returns the wanted time", authority.Actor{}.CapApprovalExpiry(req, x, later), later},
	}
	for _, c := range cases {
		if !c.got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestMayDecideAny(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner := actorOf(t, mkMember(t, 11, tA, true, "", ownerR), authority.RankOwner, adminEffective()...)
	admin := actorOf(t, mkMember(t, 13, tA, true, "", mkRole(2, tA, "Administrator", false, 10, false)), authority.RankAdmin)
	mgrApprover := actorOf(t, mkMember(t, 15, tA, true, "", mgrR), authority.RankManager, p.Roles.Approve)
	mgrPlain := actorOf(t, mkMember(t, 16, tA, true, "", mgrR), authority.RankManager, p.Crm.Leads.Read)
	user := actorOf(t, mkMember(t, 18, tA, true, "", userR), authority.RankUser, p.Crm.Leads.Read)
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	imp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, Rank: rk(authority.RankOwner), IsImpersonation: true, EffectivePermissions: adminEffective()})
	for name, c := range map[string]struct {
		a    authority.Actor
		in   tenant.ID
		want bool
	}{
		"owner":                                {owner, tA, true},
		"admin without any approve permission": {admin, tA, true},
		"manager with roles.approve":           {mgrApprover, tA, true},
		"manager without roles.approve":        {mgrPlain, tA, false},
		"user without roles.approve":           {user, tA, false},
		"owner asked about another tenant":     {owner, tB, false},
		"super user anywhere":                  {super, tB, true},
		"exempt operator in another tenant":    {op, tA, true},
		"impersonation session":                {imp, tA, false},
		"zero actor":                           {authority.Actor{}, tA, false},
	} {
		if got := c.a.MayDecideAny(c.in); got != c.want {
			t.Errorf("%s: MayDecideAny = %v, want %v", name, got, c.want)
		}
	}
}
