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

func mkMembership(n int, tid tenant.ID, active bool, reportsTo membership.ID, roles ...*role.Role) *membership.Membership {
	rids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		rids = append(rids, r.ID())
	}
	status := membership.StatusActive
	if !active {
		status = membership.StatusInactive
	}
	return membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)),
		TenantID: tid, Status: status, JoinedAt: t0, RoleAssignments: rids, ReportsTo: reportsTo})
}

func TestNewActor_MissingOrOutOfRangeRankIsRankNone(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	minus, over, max := -1, 150, 99
	for name, rank := range map[string]*int{"nil": nil, "negative": &minus, "above 99": &over, "99": &max} {
		a := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(15)), TenantID: tA, Rank: rank, EffectivePermissions: adminEffective()})
		if a.Rank() != authority.RankNone {
			t.Errorf("%s: Rank() = %d, want RankNone", name, a.Rank())
		}
		if err := a.MayActOn(owner2); !errors.Is(err, authority.ErrInsufficientRank) {
			t.Errorf("%s: MayActOn(owner) = %v, want insufficient rank", name, err)
		}
	}
	zero := 0
	if a := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(15)), TenantID: tA, Rank: &zero}); a.Rank() != authority.RankOwner {
		t.Errorf("explicit rank 0 = %d, want owner", a.Rank())
	}
}

func TestZeroActorDeniesEveryDecision(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	user := mkMember(t, 18, tA, true, "", userR)
	var zero authority.Actor
	for name, err := range map[string]error{
		"MayActOn":           zero.MayActOn(user),
		"MayChangeOverrides": zero.MayChangeOverrides(user, authority.Overrides{}),
		"MayDeactivate":      zero.MayDeactivate(user, nil),
		"MayDeleteRole":      zero.MayDeleteRole(userR),
		"MayReplaceManager":  zero.MayReplaceManager(user, owner, nil),
		"MayClearManager":    zero.MayClearManager(user, nil),
		"MayAssignRole":      zero.MayAssignRole(user, userR),
		"MayRevokeRole":      zero.MayRevokeRole(user, userR, nil),
		"MayCreateRole":      zero.MayCreateRole(tA, 60, nil, nil),
		"MayEditRole":        zero.MayEditRole(userR, authority.NoLevelChange),
		"MaySetRoleParent":   zero.MaySetRoleParent(userR, nil),
		"MayApprove":         zero.MayApprove(user, p.Crm.Leads.Read),
		"MayDeny":            zero.MayDeny(user),
	} {
		if !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("zero Actor %s = %v, want no authority", name, err)
		}
	}
	if zero.Valid() || zero.Rank() != authority.RankNone || zero.IsSuperUser() || zero.IsPlatformOperator() || zero.IsImpersonation() {
		t.Error("zero Actor must be invalid, RankNone, with no flags")
	}
}

func TestZeroMemberDeniesEveryDecision(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	user := mkMember(t, 18, tA, true, "", userR)
	report := mkMember(t, 20, tA, true, owner.ID(), userR)
	var zero authority.Member
	actors := map[string]authority.Actor{
		"owner": mkActor(t, authority.ActorInput{MembershipID: owner.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()}),
		"super": mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true}),
		"op": mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, IsPlatformOperator: true,
			EffectivePermissions: []string{p.Platform.UsersManage}}),
	}
	for who, a := range actors {
		for name, err := range map[string]error{
			"MayActOn target":       a.MayActOn(zero),
			"MayAssignRole target":  a.MayAssignRole(zero, userR),
			"MayRevokeRole target":  a.MayRevokeRole(zero, userR, nil),
			"MayChangeOverrides":    a.MayChangeOverrides(zero, authority.Overrides{}),
			"MayDeactivate":         a.MayDeactivate(zero, nil),
			"MayReplaceManager sub": a.MayReplaceManager(zero, owner, nil),
			"MayReplaceManager mgr": a.MayReplaceManager(report, zero, &owner),
			"MayReplaceManager cur": a.MayReplaceManager(report, user, &zero),
			"MayClearManager sub":   a.MayClearManager(zero, nil),
			"MayClearManager cur":   a.MayClearManager(report, &zero),
			"MayApprove requester":  a.MayApprove(zero, p.Crm.Leads.Read),
			"MayDeny requester":     a.MayDeny(zero),
			"GuardLastOwner":        authority.GuardLastOwner(zero, []authority.Member{owner}),
		} {
			if !errors.Is(err, authority.ErrNoAuthority) {
				t.Errorf("%s actor, zero Member, %s = %v, want no authority", who, name, err)
			}
		}
	}
	if zero.Valid() || zero.Rank() != authority.RankNone || zero.IsOwner() || zero.IsActive() || zero.IsSuperAdmin() {
		t.Error("zero Member must be invalid, RankNone, inactive, not owner")
	}
}

func TestMemberOf_RequiresTheFullTenantLocalRoleSet(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	foreignR := mkRole(6, tB, "Foreign", false, 50, false)
	deleted := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(7)), TenantID: tA, Name: "Gone", HierarchyLevel: 1, IsDeleted: true, CreatedAt: t0})
	stray := mkRole(8, tA, "Stray", false, 40, false)

	m := mkMembership(11, tA, true, "", ownerR, userR, deleted)
	got, err := authority.MemberOf(m, []*role.Role{ownerR, userR, deleted}, t0, activePerson())
	if err != nil || got.Rank() != authority.RankOwner || !got.IsOwner() || !got.Valid() {
		t.Fatalf("complete set: (%+v, %v)", got, err)
	}
	for name, roles := range map[string][]*role.Role{
		"nil roles (owner would look weakest)": nil,
		"owner role missing":                   {userR, deleted},
		"deleted role missing":                 {ownerR, userR},
		"unassigned extra role":                {ownerR, userR, deleted, stray},
		"nil entry":                            {ownerR, userR, deleted, nil},
	} {
		if got, err := authority.MemberOf(m, roles, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) || got.Valid() {
			t.Errorf("%s: MemberOf = (%+v, %v), want invalid Member and no authority", name, got, err)
		}
	}
	cross := mkMembership(12, tA, true, "", foreignR)
	if _, err := authority.MemberOf(cross, []*role.Role{foreignR}, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("role from another tenant: %v, want no authority", err)
	}
	if _, err := authority.MemberOf(nil, nil, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("nil membership: %v, want no authority", err)
	}
	bad := membership.UnmarshalFromDB(membership.Snapshot{ID: "not-a-uuid", PersonID: person.ID(uid(1)), TenantID: tA, Status: membership.StatusActive, JoinedAt: t0})
	if _, err := authority.MemberOf(bad, nil, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("non-uuid membership id: %v, want no authority", err)
	}
	badTenant := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(13)), PersonID: person.ID(uid(1)), TenantID: "", Status: membership.StatusActive, JoinedAt: t0})
	if _, err := authority.MemberOf(badTenant, nil, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("zero tenant id: %v, want no authority", err)
	}
	badMgr := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(13)), PersonID: person.ID(uid(1)), TenantID: tA, Status: membership.StatusActive, JoinedAt: t0, ReportsTo: "nope"})
	if _, err := authority.MemberOf(badMgr, nil, t0, activePerson()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("non-uuid manager id: %v, want no authority", err)
	}
	upper := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(strings.ToUpper(uid(13))), PersonID: person.ID(uid(1)),
		TenantID: tenant.ID(strings.ToUpper(tA.String())), Status: membership.StatusActive, JoinedAt: t0, ReportsTo: membership.ID(strings.ToUpper(uid(14)))})
	canon, err := authority.MemberOf(upper, nil, t0, activePerson())
	if err != nil || canon.ID() != membership.ID(uid(13)) || canon.TenantID() != tA || canon.ReportsTo() != membership.ID(uid(14)) {
		t.Errorf("uppercase ids not canonicalised: %+v, %v", canon, err)
	}
}

func TestCustomLevelZeroRoleHolderIsNotAnOwner(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	rogueR := mkRole(9, tA, "Rogue", false, 0, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	rogue := mkMember(t, 19, tA, true, "", rogueR)
	if rogue.IsOwner() || rogue.Rank() != authority.RankUser {
		t.Fatalf("level-0 custom holder: owner=%v rank=%d, want not owner, rank %d (user tier)", rogue.IsOwner(), rogue.Rank(), authority.RankUser)
	}
	ownerActor := mkActor(t, authority.ActorInput{MembershipID: owner.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	if err := ownerActor.MayActOn(rogue); err != nil {
		t.Errorf("owner acts on a custom level-0 holder: %v", err)
	}
	rogueActor := mkActor(t, authority.ActorInput{MembershipID: rogue.ID(), TenantID: tA, Rank: rk(authority.RankUser), EffectivePermissions: adminEffective()})
	if err := rogueActor.MayActOn(owner); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("custom level-0 holder acts on a real owner: %v, want insufficient rank", err)
	}
}

func TestMayCreateRole_TenantAndInitialPermissions(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	admin := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(13)), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(50), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.RolesManage}})
	cases := map[string]struct {
		got  error
		want error
	}{
		"admin, own tenant":                        {admin.MayCreateRole(tA, 11, []string{p.Crm.Leads.Read}, nil), nil},
		"admin, other tenant":                      {admin.MayCreateRole(tB, 11, nil, nil), authority.ErrNoAuthority},
		"admin, tenant.admin in initial perms":     {admin.MayCreateRole(tA, 11, []string{p.Meta.TenantAdmin}, nil), authority.ErrGrantExceedsAuthority},
		"admin, reserved in initial perms":         {admin.MayCreateRole(tA, 11, []string{p.Platform.TenantsManage}, nil), authority.ErrGrantExceedsAuthority},
		"admin, unknown in initial perms":          {admin.MayCreateRole(tA, 11, []string{"nope"}, nil), authority.ErrGrantExceedsAuthority},
		"admin, level above 99":                    {admin.MayCreateRole(tA, 100, nil, nil), authority.ErrInsufficientRank},
		"admin, negative level":                    {admin.MayCreateRole(tA, -1, nil, nil), authority.ErrInsufficientRank},
		"super, other tenant":                      {super.MayCreateRole(tB, 11, []string{p.Platform.TenantsManage}, nil), nil},
		"exempt operator, other tenant, level 11":  {op.MayCreateRole(tB, 11, []string{p.Meta.TenantAdmin}, nil), nil},
		"exempt operator, own tenant follows rank": {op.MayCreateRole(tP, 11, nil, nil), authority.ErrInsufficientRank},
	}
	for name, c := range cases {
		if c.want == nil && c.got != nil || c.want != nil && !errors.Is(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", name, c.got, c.want)
		}
	}
}
