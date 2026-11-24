package authority

import (
	"fmt"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Hierarchy round 4, I2: the actor the policy decides with is narrowed by the
// actor's freshly loaded membership. These tests pin the rule itself (more
// junior rank, intersection of permissions, deny on a missing / foreign /
// inactive membership) and which actors need it at all.

const freshMemberID = "0190a000-0000-7000-8000-0000000000a1"

var freshNow = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func freshRole(n int, tid tenant.ID, level int, sys bool) *role.Role {
	name := fmt.Sprintf("Fresh Role %d", n)
	if sys && level == 0 {
		name = role.SystemRoles.Tenant.CompanyOwner
	}
	return role.UnmarshalFromDB(role.Snapshot{ID: role.ID(fmt.Sprintf("0190a000-0000-7000-8000-%012d", 900+n)), TenantID: tid,
		Name: name, IsSystemDefault: sys, HierarchyLevel: level, IsActive: true})
}

func freshMemberOf(t *testing.T, id membership.ID, tid tenant.ID, status membership.Status, state PersonState, roles ...*role.Role) Member {
	t.Helper()
	ids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		ids = append(ids, r.ID())
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: id, PersonID: person.ID("0190a000-0000-7000-8000-0000000000b1"),
		TenantID: tid, Status: status, JoinedAt: freshNow, RoleAssignments: ids})
	out, err := MemberOf(m, roles, freshNow, state)
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

func canAct() PersonState { return NewPersonState(true, false) }

func tokenActor(t *testing.T, rank Rank, perms ...string) Actor {
	t.Helper()
	r := rank.Int()
	return newActorT(t, ActorInput{MembershipID: freshMemberID, TenantID: tenantOne, Rank: &r, EffectivePermissions: perms})
}

func TestNarrowedBy_RankIsTheMoreJuniorOfTokenAndFresh(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		token Rank
		fresh int // role level held now; -1 = no role
		want  Rank
	}{
		"token owner, fresh admin: demoted to admin":         {RankOwner, 10, RankAdmin},
		"token owner, fresh owner: unchanged":                {RankOwner, 0, RankOwner},
		"token admin, fresh manager: demoted":                {RankAdmin, 20, RankManager},
		"token admin, fresh no role: demoted to none":        {RankAdmin, -1, RankNone},
		"token manager, fresh admin: promoted, NOT honoured": {RankManager, 10, RankManager},
		"token none, fresh owner: never gains":               {RankNone, 0, RankNone},
		"equal rank":                                         {RankTeamLead, 30, RankTeamLead},
	} {
		var roles []*role.Role
		if c.fresh >= 0 {
			roles = append(roles, freshRole(1, tenantOne, c.fresh, c.fresh == 0))
		}
		fresh := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct(), roles...)
		got := tokenActor(t, c.token).NarrowedBy(fresh, nil, nil)
		if !got.Valid() || got.Rank() != c.want {
			t.Errorf("%s: rank = %d (valid %v), want %d", name, got.Rank(), got.Valid(), c.want)
		}
	}
}

func TestNarrowedBy_PermissionsAreTheIntersection(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	fresh := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct())
	tok := tokenActor(t, RankManager, p.Crm.Leads.Read, p.Crm.Leads.Manage, p.Users.View)
	got := tok.NarrowedBy(fresh, []string{p.Crm.Leads.Read, p.Users.View, p.Crm.Leads.Assign}, nil)
	for name, c := range map[string]struct {
		perm string
		want bool
	}{
		"in both":                   {p.Crm.Leads.Read, true},
		"in both (second)":          {p.Users.View, true},
		"token only: revoked since": {p.Crm.Leads.Manage, false},
		"fresh only: granted since": {p.Crm.Leads.Assign, false},
		"in neither":                {p.Roles.Manage, false},
		"unknown name never held":   {"not.a.permission", false},
		"empty name never held":     {"", false},
	} {
		if got.hasOwn(c.perm) != c.want {
			t.Errorf("%s: hasOwn(%q) = %v, want %v", name, c.perm, !c.want, c.want)
		}
	}
	// An empty fresh set removes everything.
	if none := tok.NarrowedBy(fresh, nil, nil); none.hasOwn(p.Crm.Leads.Read) {
		t.Error("empty fresh set kept a token permission")
	}
}

func TestNarrowedBy_TenantAdminIsExpandedOnBothSides(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	fresh := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct())
	admin := tokenActor(t, RankAdmin, p.Meta.TenantAdmin)
	// Fresh still has tenant.admin: every implied name survives.
	if got := admin.NarrowedBy(fresh, []string{p.Meta.TenantAdmin}, nil); !got.hasOwn(p.Crm.Leads.ReadAll) || !got.hasOwn(p.Meta.TenantAdmin) {
		t.Error("tenant.admin on both sides lost an implied permission")
	}
	// Fresh lost tenant.admin but still holds one implied name individually: only that one survives.
	got := admin.NarrowedBy(fresh, []string{p.Crm.Leads.ReadAll}, nil)
	if got.hasOwn(p.Meta.TenantAdmin) || got.hasOwn(p.Users.Create) || !got.hasOwn(p.Crm.Leads.ReadAll) {
		t.Error("a revoked tenant.admin kept its implied permissions")
	}
	// The token had no tenant.admin; fresh gained it: nothing is gained.
	plain := tokenActor(t, RankAdmin, p.Crm.Leads.Read)
	if got := plain.NarrowedBy(fresh, []string{p.Meta.TenantAdmin}, nil); got.hasOwn(p.Users.Create) || got.hasOwn(p.Meta.TenantAdmin) {
		t.Error("a token gained permissions the fresh facts hold")
	}
}

func TestNarrowedBy_DeniesWhenTheFreshMembershipCannotActForThisToken(t *testing.T) {
	t.Parallel()
	owner := freshRole(1, tenantOne, 0, true)
	ok := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct(), owner)
	other := freshMemberOf(t, "0190a000-0000-7000-8000-0000000000a2", tenantOne, membership.StatusActive, canAct(), owner)
	foreign := freshMemberOf(t, freshMemberID, tenantTwo, membership.StatusActive, canAct(), freshRole(2, tenantTwo, 0, true))
	inactive := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusInactive, canAct(), owner)
	suspended := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, NewPersonState(true, true), owner)
	deactivatedPerson := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, NewPersonState(false, false), owner)
	unknownPerson := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, PersonState{}, owner)

	admin := permission.IdentityPermissions.Meta.TenantAdmin
	tok := tokenActor(t, RankOwner, admin)
	if !tok.NarrowedBy(ok, []string{admin}, nil).Valid() {
		t.Fatal("sanity: the matching active membership must keep the actor valid")
	}
	for name, fresh := range map[string]Member{
		"zero member":               {},
		"another membership":        other,
		"same id in another tenant": foreign,
		"inactive membership":       inactive,
		"globally suspended person": suspended,
		"deactivated person":        deactivatedPerson,
		"person state not loaded":   unknownPerson,
	} {
		if got := tok.NarrowedBy(fresh, []string{admin}, nil); got.Valid() {
			t.Errorf("%s: actor stays valid, want the zero actor", name)
		}
	}
	if (Actor{}).NarrowedBy(ok, nil, nil).Valid() {
		t.Error("the zero actor became valid")
	}
}

func TestNarrowedBy_KeepsTheFlagsThatAreNotMembershipFacts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	fresh := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct())
	op := newActorT(t, ActorInput{MembershipID: freshMemberID, TenantID: tenantOne, IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	got := op.NarrowedBy(fresh, []string{p.Platform.UsersManage}, nil)
	if !got.IsPlatformOperator() || got.IsSuperUser() || got.IsImpersonation() || got.MembershipID() != op.MembershipID() || got.TenantID() != op.TenantID() {
		t.Errorf("flags or identity changed: %+v", got)
	}
	// A narrowed operator that lost its platform permission is no longer exempt anywhere.
	if lost := op.NarrowedBy(fresh, nil, nil); lost.exemptOperatorIn(tenantTwo) {
		t.Error("an operator whose platform permission is gone is still exempt")
	}
}

func TestNeedsFreshFacts(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	r := RankAdmin.Int()
	mk := func(in ActorInput) Actor {
		in.MembershipID, in.TenantID, in.Rank = freshMemberID, tenantOne, &r
		return newActorT(t, in)
	}
	opManage := []string{p.Platform.UsersManage}
	for name, c := range map[string]struct {
		actor  Actor
		tenant tenant.ID
		want   bool
	}{
		"ordinary member, own tenant":                      {mk(ActorInput{}), tenantOne, true},
		"ordinary member, another tenant":                  {mk(ActorInput{}), tenantTwo, true},
		"super user, own tenant (re-read)":                 {mk(ActorInput{IsSuperUser: true}), tenantOne, true},
		"super user, another tenant (bounded by stamp)":    {mk(ActorInput{IsSuperUser: true}), tenantTwo, false},
		"super user, tenant id that does not canonicalise": {mk(ActorInput{IsSuperUser: true}), "garbage", false},
		"impersonation session":                            {mk(ActorInput{IsImpersonation: true}), tenantOne, false},
		"exempt operator in another tenant":                {mk(ActorInput{IsPlatformOperator: true, EffectivePermissions: opManage}), tenantTwo, false},
		"operator in its OWN tenant follows rank":          {mk(ActorInput{IsPlatformOperator: true, EffectivePermissions: opManage}), tenantOne, true},
		"operator without the exemption permission":        {mk(ActorInput{IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.TenantsView}}), tenantTwo, true},
		"operator, tenant id that does not canonicalise":   {mk(ActorInput{IsPlatformOperator: true, EffectivePermissions: opManage}), "garbage", true},
		"zero actor": {Actor{}, tenantOne, false},
	} {
		if got := c.actor.NeedsFreshFacts(c.tenant); got != c.want {
			t.Errorf("%s: NeedsFreshFacts = %v, want %v", name, got, c.want)
		}
	}
}

// A super user acting in their own tenant is decided on the stored membership: the
// super-admin role must still be held (the token's flag is a fact of the membership).
func TestNarrowedBy_ASuperUserMustStillHoldTheSuperAdminRole(t *testing.T) {
	t.Parallel()
	superRole := role.UnmarshalFromDB(role.Snapshot{ID: role.ID("0190a000-0000-7000-8000-000000000990"), TenantID: tenantOne,
		Name: "Super Admin", IsSystemDefault: true, IsSuperAdmin: true, HierarchyLevel: 0, IsActive: true})
	plain := freshRole(1, tenantOne, 10, false)
	r := RankOwner.Int()
	super := newActorT(t, ActorInput{MembershipID: freshMemberID, TenantID: tenantOne, Rank: &r, IsSuperUser: true})

	holds := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct(), superRole)
	if got := super.NarrowedBy(holds, nil, nil); !got.Valid() || !got.IsSuperUser() {
		t.Errorf("a super admin who still holds the role: valid=%v super=%v, want a valid super user", got.Valid(), got.IsSuperUser())
	}
	lost := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct(), plain)
	if got := super.NarrowedBy(lost, nil, nil); got.Valid() {
		t.Error("a super user whose super-admin role was removed still acts")
	}
	deleted := role.UnmarshalFromDB(role.Snapshot{ID: superRole.ID(), TenantID: tenantOne, Name: "Super Admin", IsSuperAdmin: true})
	gone := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusActive, canAct(), deleted)
	if got := super.NarrowedBy(gone, nil, nil); got.Valid() {
		t.Error("a super user whose super-admin role was deleted still acts")
	}
	inactive := freshMemberOf(t, freshMemberID, tenantOne, membership.StatusInactive, canAct(), superRole)
	if got := super.NarrowedBy(inactive, nil, nil); got.Valid() {
		t.Error("a deactivated super user still acts")
	}
	// An ordinary actor never needs the role.
	ordinary := tokenActor(t, RankAdmin)
	if got := ordinary.NarrowedBy(lost, nil, nil); !got.Valid() {
		t.Error("the super-admin requirement leaked onto an ordinary actor")
	}
}
