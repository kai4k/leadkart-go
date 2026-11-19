package authority_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const (
	tA = tenant.ID("0190a000-0000-7000-8000-00000000000a")
	tB = tenant.ID("0190a000-0000-7000-8000-00000000000b")
	tP = tenant.ID("0190a000-0000-7000-8000-00000000000f")
)

var t0 = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

func uid(n int) string { return fmt.Sprintf("0190a000-0000-7000-8000-%012d", n) }

func mkRole(n int, tid tenant.ID, name string, sys bool, level int, super bool, perms ...string) *role.Role {
	ps := make([]*permission.Permission, 0, len(perms))
	for _, p := range perms {
		ps = append(ps, permission.FromConstant(p))
	}
	return role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(n)), TenantID: tid, Name: name,
		IsSystemDefault: sys, IsSuperAdmin: super, HierarchyLevel: level, Permissions: ps, CreatedAt: t0})
}

func mkMember(t *testing.T, n int, tid tenant.ID, active bool, reportsTo membership.ID, roles ...*role.Role) authority.Member {
	ids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		ids = append(ids, r.ID())
	}
	t.Helper()
	status := membership.StatusActive
	if !active {
		status = membership.StatusInactive
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)),
		TenantID: tid, Status: status, JoinedAt: t0, RoleAssignments: ids, ReportsTo: reportsTo})
	out, err := authority.MemberOf(m, roles)
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

// rk is the *int the optional rank claim takes.
func rk(r authority.Rank) *int { n := int(r); return &n }

func adminEffective() []string {
	out := []string{permission.IdentityPermissions.Meta.TenantAdmin}
	for _, p := range permission.All() {
		if permission.ImpliedByTenantAdmin(p.Name()) {
			out = append(out, p.Name())
		}
	}
	return out
}

func mkActor(t *testing.T, in authority.ActorInput) authority.Actor {
	t.Helper()
	a, err := authority.NewActor(in)
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

func TestPolicy_DecisionTable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	tlR := mkRole(4, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Crm.Leads.Read, p.Roles.Approve)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false, p.Crm.Leads.Read)
	assignR := mkRole(6, tA, "Lead Router", false, 40, false, p.Crm.Leads.Assign)
	superR := mkRole(7, tA, "SuperAdmin", true, 0, true)

	owner1 := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	admin2 := mkMember(t, 14, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	mgr2 := mkMember(t, 16, tA, true, "", mgrR)
	tl := mkMember(t, 17, tA, true, mgr.ID(), tlR)
	user := mkMember(t, 18, tA, true, tl.ID(), userR)
	puppet := mkMember(t, 19, tA, true, mgr.ID())
	staleAdmin := mkMember(t, 20, tA, true, mgr.ID(), adminR) // upward link left over from an old rank
	inactiveMgr := mkMember(t, 21, tA, false, "", mgrR)
	userRB := mkRole(25, tB, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	foreign := mkMember(t, 22, tB, true, "", userRB)
	superMember := mkMember(t, 23, tA, true, "", superR)

	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner1.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	aMgr := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: rk(authority.RankManager), EffectivePermissions: []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve}})
	aMgrNoApprove := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: rk(authority.RankManager), EffectivePermissions: []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll}})
	aTL := mkActor(t, authority.ActorInput{MembershipID: tl.ID(), TenantID: tA, Rank: rk(authority.RankTeamLead), EffectivePermissions: []string{p.Crm.Leads.Read, p.Roles.Approve}})
	aUser := mkActor(t, authority.ActorInput{MembershipID: user.ID(), TenantID: tA, Rank: rk(authority.RankUser), EffectivePermissions: []string{p.Crm.Leads.Read}})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, Rank: rk(authority.RankOwner), IsSuperUser: true})
	aOp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	aOpNoManage := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(92)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.TenantsView}})
	aImp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, Rank: rk(authority.RankOwner), IsImpersonation: true, EffectivePermissions: adminEffective()})
	var zero authority.Actor

	cases := []struct {
		name string
		got  error
		want error
	}{
		// H2 — act only downward
		{"zero actor", zero.MayActOn(user), authority.ErrNoAuthority},
		{"owner on owner", aOwner.MayActOn(owner2), nil},
		{"owner on admin", aOwner.MayActOn(admin), nil},
		{"admin on owner", aAdmin.MayActOn(owner1), authority.ErrInsufficientRank},
		{"admin on admin (lateral)", aAdmin.MayActOn(admin2), authority.ErrInsufficientRank},
		{"admin on manager", aAdmin.MayActOn(mgr), nil},
		{"team lead on user", aTL.MayActOn(user), nil},
		{"user on team lead (upward)", aUser.MayActOn(tl), authority.ErrInsufficientRank},
		{"self", aAdmin.MayActOn(admin), authority.ErrSelfTarget},
		{"cross tenant", aAdmin.MayActOn(foreign), authority.ErrNoAuthority},
		{"super-admin target", aOwner.MayActOn(superMember), authority.ErrInsufficientRank},
		{"super on super-admin", aSuper.MayActOn(superMember), nil},
		{"exempt operator on owner", aOp.MayActOn(owner1), nil},
		{"operator without manage", aOpNoManage.MayActOn(user), authority.ErrNoAuthority},
		{"impersonation on owner", aImp.MayActOn(owner1), authority.ErrInsufficientRank},
		{"impersonation on manager", aImp.MayActOn(mgr), nil},
		// H3 — grant ceiling
		{"owner confers tenant.admin", aOwner.MayChangePermissions(tA, []string{p.Meta.TenantAdmin}), nil},
		{"admin confers tenant.admin", aAdmin.MayChangePermissions(tA, []string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"admin confers read_all", aAdmin.MayChangePermissions(tA, []string{p.Crm.Leads.ReadAll}), nil},
		{"manager confers unheld", aMgr.MayChangePermissions(tA, []string{p.Crm.Leads.Assign}), authority.ErrGrantExceedsAuthority},
		{"manager confers held", aMgr.MayChangePermissions(tA, []string{p.Crm.Leads.ReadAll}), nil},
		{"owner confers reserved", aOwner.MayChangePermissions(tA, []string{p.Platform.TenantsManage}), authority.ErrGrantExceedsAuthority},
		{"super confers reserved", aSuper.MayChangePermissions(tA, []string{p.Platform.TenantsManage}), nil},
		{"operator confers tenant.admin", aOp.MayChangePermissions(tA, []string{p.Meta.TenantAdmin}), nil},
		{"operator confers anonymise", aOp.MayChangePermissions(tA, []string{p.Users.Anonymise}), authority.ErrGrantExceedsAuthority},
		{"impersonation confers tenant.admin", aImp.MayChangePermissions(tA, []string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"impersonation confers crm", aImp.MayChangePermissions(tA, []string{p.Crm.Leads.Read}), nil},
		{"zero confers", zero.MayChangePermissions(tA, nil), authority.ErrNoAuthority},
		// H3 — roles
		{"owner assigns owner role", aOwner.MayAssignRole(admin, ownerR), nil},
		{"admin assigns admin role", aAdmin.MayAssignRole(mgr, adminR), authority.ErrInsufficientRank},
		{"admin assigns manager role", aAdmin.MayAssignRole(user, mgrR), nil},
		{"manager assigns team lead role", aMgr.MayAssignRole(user, tlR), nil},
		{"manager assigns manager role", aMgr.MayAssignRole(user, mgrR), authority.ErrInsufficientRank},
		{"manager assigns role with unheld perm", aMgr.MayAssignRole(user, assignR), authority.ErrGrantExceedsAuthority},
		{"admin assigns super role", aAdmin.MayAssignRole(user, superR), authority.ErrGrantExceedsAuthority},
		{"impersonation assigns owner role", aImp.MayAssignRole(mgr, ownerR), authority.ErrInsufficientRank},
		{"operator assigns owner role", aOp.MayAssignRole(user, ownerR), nil},
		{"admin revokes admin role from admin", aAdmin.MayRevokeRole(admin2, adminR), authority.ErrInsufficientRank},
		{"admin creates level 11", aAdmin.MayCreateRole(tA, 11, nil), nil},
		{"admin creates level 10", aAdmin.MayCreateRole(tA, 10, nil), authority.ErrInsufficientRank},
		{"manager creates level 20", aMgr.MayCreateRole(tA, 20, nil), authority.ErrInsufficientRank},
		{"admin relevels manager role to 15", aAdmin.MayEditRole(mgrR, 15, nil), nil},
		{"admin relevels manager role to 5", aAdmin.MayEditRole(mgrR, 5, nil), authority.ErrInsufficientRank},
		{"admin edits admin role", aAdmin.MayEditRole(adminR, authority.NoLevelChange, nil), authority.ErrInsufficientRank},
		{"owner edits owner role", aOwner.MayEditRole(ownerR, authority.NoLevelChange, nil), authority.ErrInsufficientRank},
		{"manager adds held perm to TL role", aMgr.MayEditRole(tlR, authority.NoLevelChange, []string{p.Crm.Leads.ReadAll}), nil},
		{"manager adds unheld perm to TL role", aMgr.MayEditRole(tlR, authority.NoLevelChange, []string{p.Crm.Leads.Assign}), authority.ErrGrantExceedsAuthority},
		{"admin adds tenant.admin to TL role", aAdmin.MayEditRole(tlR, authority.NoLevelChange, []string{p.Meta.TenantAdmin}), authority.ErrGrantExceedsAuthority},
		{"parent more senior", aAdmin.MaySetRoleParent(tlR, mgrR), nil},
		{"parent not more senior", aAdmin.MaySetRoleParent(mgrR, tlR), authority.ErrRoleParentNotEligible},
		{"clear parent", aAdmin.MaySetRoleParent(tlR, nil), nil},
		{"junior edits parent", aTL.MaySetRoleParent(mgrR, adminR), authority.ErrInsufficientRank},
		// H4 — managers
		{"admin links user to manager", aAdmin.MaySetManager(user, mgr), nil},
		{"upward link", aAdmin.MaySetManager(mgr, user), authority.ErrManagerNotEligible},
		{"lateral link", aAdmin.MaySetManager(mgr, mgr2), authority.ErrManagerNotEligible},
		{"inactive manager", aAdmin.MaySetManager(user, inactiveMgr), authority.ErrManagerNotEligible},
		{"manager appoints self", aMgr.MaySetManager(puppet, mgr), nil},
		{"team lead appoints senior", aTL.MaySetManager(user, mgr), authority.ErrInsufficientRank},
		{"admin unlinks from manager", aAdmin.MayUnlinkManager(tl, mgr), nil},
		{"team lead unlinks own report", aTL.MayUnlinkManager(user, tl), nil},
		{"team lead unlinks senior's report", aTL.MayUnlinkManager(user, mgr), authority.ErrInsufficientRank},
		// H4 — approvals
		{"manager approves report held perm", aMgr.MayApprove(tl, p.Crm.Leads.ReadAll), nil},
		{"manager approves unheld perm", aMgr.MayApprove(tl, p.Crm.Leads.Assign), authority.ErrGrantExceedsAuthority},
		{"manager approves puppet", aMgr.MayApprove(puppet, p.Crm.Leads.Read), nil},
		{"manager without roles.approve", aMgrNoApprove.MayApprove(tl, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"manager not the requester's manager", aMgr.MayApprove(user, p.Crm.Leads.Read), authority.ErrNotApprover},
		{"stale upward link", aMgr.MayApprove(staleAdmin, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"admin approves anyone below", aAdmin.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"admin approves owner", aAdmin.MayApprove(owner1, p.Crm.Leads.Read), authority.ErrInsufficientRank},
		{"tenant.admin not requestable", aOwner.MayApprove(user, p.Meta.TenantAdmin), authority.ErrGrantExceedsAuthority},
		{"self approval", aUser.MayApprove(user, p.Crm.Leads.Read), authority.ErrSelfTarget},
		{"impersonation approves", aImp.MayApprove(user, p.Crm.Leads.Read), authority.ErrImpersonationNotAllowed},
		{"operator approves", aOp.MayApprove(user, p.Crm.Leads.Assign), nil},
		{"manager denies report", aMgr.MayDeny(tl), nil},
		{"manager denies non-report", aMgr.MayDeny(user), authority.ErrNotApprover},
		{"admin denies", aAdmin.MayDeny(user), nil},
		// H6 — guards
		{"last owner", authority.GuardLastOwner(owner1, []membership.ID{owner1.ID()}), authority.ErrLastOwnerProtected},
		{"second owner remains", authority.GuardLastOwner(owner1, []membership.ID{owner1.ID(), owner2.ID()}), nil},
		{"non-owner target", authority.GuardLastOwner(admin, []membership.ID{owner1.ID()}), nil},
		{"owner keeps tenant.admin", authority.GuardOwnerOverrides(owner2, []string{p.Meta.TenantAdmin}), authority.ErrOwnerProtected},
		{"owner other revoke ok", authority.GuardOwnerOverrides(owner2, []string{p.Crm.Leads.Read}), nil},
		{"admin may lose tenant.admin override", authority.GuardOwnerOverrides(admin, []string{p.Meta.TenantAdmin}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.want == nil && tc.got != nil {
				t.Fatalf("got %v, want nil", tc.got)
			}
			if tc.want != nil && !errors.Is(tc.got, tc.want) {
				t.Fatalf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

func TestRank_TierClampAndRankOf(t *testing.T) {
	t.Parallel()
	for lvl, want := range map[int]authority.Tier{0: authority.TierOwner, 5: authority.TierAdmin, 10: authority.TierAdmin,
		15: authority.TierManager, 20: authority.TierManager, 25: authority.TierTeamLead, 30: authority.TierTeamLead, 50: authority.TierUser, 99: authority.TierUser} {
		if got := authority.Rank(lvl).Tier(); got != want {
			t.Errorf("Rank(%d).Tier() = %s, want %s", lvl, got, want)
		}
	}
	if authority.ClampRank(-1) != authority.RankNone || authority.ClampRank(150) != authority.RankNone || authority.ClampRank(20) != authority.RankManager {
		t.Fatal("ClampRank must fail closed outside [0,99]")
	}
	deleted := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(1)), TenantID: tA, Name: "Gone", HierarchyLevel: 1, IsDeleted: true})
	if got := authority.RankOf([]*role.Role{mkRole(2, tA, "M", false, 20, false), mkRole(3, tA, "U", false, 50, false), deleted}); got != authority.RankManager {
		t.Fatalf("RankOf = %d, want 20 (deleted roles ignored)", got)
	}
	if authority.RankOf(nil) != authority.RankNone {
		t.Fatal("no roles → RankNone")
	}
}

func TestNewActor_ValidationAndImpersonationRank(t *testing.T) {
	t.Parallel()
	if _, err := authority.NewActor(authority.ActorInput{TenantID: tA}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("missing membership: %v", err)
	}
	if _, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tA, IsImpersonation: true, IsPlatformOperator: true}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("impersonating operator: %v", err)
	}
	imp := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tA, Rank: rk(authority.RankOwner), IsImpersonation: true,
		EffectivePermissions: []string{"not.a.permission", permission.IdentityPermissions.Crm.Leads.Read}})
	if imp.Rank() != authority.RankAdmin {
		t.Fatalf("impersonation rank = %d, want Admin", imp.Rank())
	}
	if imp.MayChangePermissions(tA, []string{"not.a.permission"}) == nil || imp.MayChangePermissions(tA, []string{permission.IdentityPermissions.Crm.Leads.Read}) != nil {
		t.Fatal("unknown names dropped, known kept")
	}
	var zero authority.Actor
	if zero.Valid() || zero.Rank() != authority.RankNone {
		t.Fatal("zero Actor must hold nothing and rank RankNone")
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	got := authority.Delta([]string{"a", "b", "b"}, []string{"b", "c"})
	if fmt.Sprint(got) != "[a c]" {
		t.Fatalf("Delta = %v, want [a c]", got)
	}
}
