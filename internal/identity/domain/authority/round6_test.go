package authority_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Hierarchy round 6: conferral needs a durable hold, the Overrides equality that
// makes a no-op a no-op, and the pins for the holder / reporting / time-bound
// rules that earlier rounds left to mutants.

// mkMemberState is mkMember with an explicit person state (locked, suspended).
func mkMemberState(t *testing.T, n int, state authority.PersonState, reportsTo membership.ID, roles ...*role.Role) authority.Member {
	t.Helper()
	rids := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		rids = append(rids, r.ID())
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)),
		TenantID: tA, Status: membership.StatusActive, JoinedAt: t0, RoleAssignments: rids, ReportsTo: reportsTo})
	out, err := authority.MemberOf(m, roles, t0, state)
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

// narrowedManager is an actor whose stored membership holds `granted` (overrides)
// and the permissions of its roles (viaRoles); the token lists `effective`.
func narrowedManager(t *testing.T, n int, rank authority.Rank, roles []*role.Role, granted []membership.GrantedOverride, viaRoles []string, effective ...string) authority.Actor {
	t.Helper()
	fresh := mkMemberOv(t, n, tA, granted, nil, roles...)
	token := mkActor(t, authority.ActorInput{MembershipID: fresh.ID(), TenantID: tA, Rank: rk(rank), EffectivePermissions: effective})
	a := token.NarrowedBy(fresh, effective, viaRoles)
	if !a.Valid() {
		t.Fatal("narrowed actor invalid")
	}
	return a
}

func timedGrant(name string, at time.Time) membership.GrantedOverride {
	return membership.GrantedOverride{Permission: permission.FromConstant(name), ExpiresAt: at}
}

// TestConferral_NeedsADurableHold: an actor who holds a name only through a
// time-bound grant cannot hand it to anyone, by any route; a durable holder can;
// and removing a name is not conferring it.
func TestConferral_NeedsADurableHold(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	soon := t0.Add(24 * time.Hour)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Roles.Update)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	carrier := mkRole(40, tA, "Carries X", false, 45, false, x)
	custom := mkRole(41, tA, "Custom", false, 40, false)
	customHasX := mkRole(42, tA, "Custom with X", false, 40, false, x)
	target := mkMember(t, 18, tA, true, "", userR)
	holder := mkMember(t, 19, tA, true, "", custom)
	timedOnly := narrowedManager(t, 15, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timedGrant(x, soon)}, []string{p.Roles.Update}, p.Roles.Update, x)
	durable := narrowedManager(t, 16, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{permanent(x)}, []string{p.Roles.Update}, p.Roles.Update, x)
	viaRole := narrowedManager(t, 17, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timedGrant(x, soon)}, []string{p.Roles.Update, x}, p.Roles.Update, x)

	grantX := authority.Overrides{Granted: []authority.Override{{Name: x}}}
	hasTimedX := mkMemberOv(t, 23, tA, []membership.GrantedOverride{timedGrant(x, soon)}, nil, userR)
	hasPermanentX := mkMemberOv(t, 21, tA, []membership.GrantedOverride{permanent(x)}, nil, userR)
	carrierHolder := mkMember(t, 22, tA, true, "", userR, carrier)
	cases := []decision{
		// the overrides route
		{"timed holder grants X by overrides", timedOnly.MayChangeOverrides(target, grantX), authority.ErrGrantExceedsAuthority},
		{"timed holder grants X with an expiry by overrides", timedOnly.MayChangeOverrides(target, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: soon}}}), authority.ErrGrantExceedsAuthority},
		{"durable holder grants X by overrides", durable.MayChangeOverrides(target, grantX), nil},
		{"holder through a role grants X by overrides", viaRole.MayChangeOverrides(target, grantX), nil},
		// a role assignment of a role carrying X
		{"timed holder assigns a role carrying X", timedOnly.MayAssignRole(target, carrier), authority.ErrGrantExceedsAuthority},
		{"durable holder assigns a role carrying X", durable.MayAssignRole(target, carrier), nil},
		// the role's permission set
		{"timed holder grants X to a role", timedOnly.MayGrantRolePermission(custom, x, hs(holder)), authority.ErrGrantExceedsAuthority},
		{"durable holder grants X to a role", durable.MayGrantRolePermission(custom, x, hs(holder)), nil},
		{"timed holder grants a name the role already carries (nothing conferred)", timedOnly.MayGrantRolePermission(customHasX, x, hs(holder)), nil},
		{"timed holder replaces a role's set with X added", timedOnly.MayReplaceRolePermissions(custom, []string{x}, hs(holder)), authority.ErrGrantExceedsAuthority},
		{"durable holder replaces a role's set with X added", durable.MayReplaceRolePermissions(custom, []string{x}, hs(holder)), nil},
		{"timed holder replaces a set that already carries X, keeping it", timedOnly.MayReplaceRolePermissions(customHasX, []string{x}, hs(holder)), nil},
		{"timed holder creates a role carrying X", timedOnly.MayCreateRole(tA, 45, []string{x}, nil), authority.ErrGrantExceedsAuthority},
		{"durable holder creates a role carrying X", durable.MayCreateRole(tA, 45, []string{x}, nil), nil},
		// removing is not conferring
		{"timed holder revokes X from a role", timedOnly.MayRevokeRolePermission(customHasX, x, hs(holder)), nil},
		{"timed holder removes X by overrides", timedOnly.MayChangeOverrides(hasPermanentX, authority.Overrides{}), nil},
		{"timed holder revokes a role carrying X", timedOnly.MayRevokeRole(carrierHolder, carrier, nil), nil},
		// extending or making permanent a grant the target already has is conferring; shortening is not
		{"timed holder extends a timed grant", timedOnly.MayChangeOverrides(hasTimedX, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: soon.Add(time.Hour)}}}), authority.ErrGrantExceedsAuthority},
		{"timed holder makes a timed grant permanent", timedOnly.MayChangeOverrides(hasTimedX, grantX), authority.ErrGrantExceedsAuthority},
		{"timed holder shortens a timed grant", timedOnly.MayChangeOverrides(hasTimedX, authority.Overrides{Granted: []authority.Override{{Name: x, ExpiresAt: soon.Add(-time.Hour)}}}), nil},
	}
	// lifting a revoke confers the name back
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false)
	adminTimed := narrowedManager(t, 14, authority.RankAdmin, []*role.Role{adminR}, []membership.GrantedOverride{timedGrant(x, soon)}, nil, x)
	revokedX := mkMemberOv(t, 24, tA, nil, []string{x}, userR)
	cases = append(cases, decision{"timed holder lifts a revoke of X", adminTimed.MayChangeOverrides(revokedX, authority.Overrides{}), authority.ErrGrantExceedsAuthority})
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

func TestOverrides_Equal(t *testing.T) {
	t.Parallel()
	soon := t0.Add(time.Hour)
	g := func(name string, at time.Time) authority.Override {
		return authority.Override{Name: name, ExpiresAt: at}
	}
	a := authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("b", soon)}, Revoked: []string{"x", "y"}}
	for name, c := range map[string]struct {
		other authority.Overrides
		want  bool
	}{
		"same":                    {authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("b", soon)}, Revoked: []string{"x", "y"}}, true},
		"same, other order":       {authority.Overrides{Granted: []authority.Override{g("b", soon), g("a", time.Time{})}, Revoked: []string{"y", "x"}}, true},
		"expiry differs":          {authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("b", soon.Add(time.Second))}, Revoked: []string{"x", "y"}}, false},
		"permanent vs timed":      {authority.Overrides{Granted: []authority.Override{g("a", soon), g("b", soon)}, Revoked: []string{"x", "y"}}, false},
		"a granted name missing":  {authority.Overrides{Granted: []authority.Override{g("a", time.Time{})}, Revoked: []string{"x", "y"}}, false},
		"a granted name replaced": {authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("c", soon)}, Revoked: []string{"x", "y"}}, false},
		"a revoked name missing":  {authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("b", soon)}, Revoked: []string{"x"}}, false},
		"a revoked name replaced": {authority.Overrides{Granted: []authority.Override{g("a", time.Time{}), g("b", soon)}, Revoked: []string{"x", "z"}}, false},
	} {
		if got := a.Equal(c.other); got != c.want {
			t.Errorf("%s: Equal = %v, want %v", name, got, c.want)
		}
	}
	if !(authority.Overrides{}).Equal(authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}) {
		t.Error("nil and empty must be equal")
	}
}

// TestTimeBoundHoldings_Pins: a permanent tenant.admin override is never capped;
// a name held by a direct timed grant AND by a timed tenant.admin uses the LATER date.
func TestTimeBoundHoldings_Pins(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Tasks.WorkItems.Reassign
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Roles.Approve)
	req := mkMember(t, 18, tA, true, "", mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false))
	soon, mid, later := t0.Add(24*time.Hour), t0.Add(48*time.Hour), t0.Add(90*24*time.Hour)
	ta := p.Meta.TenantAdmin

	permanentAdmin := narrowedManager(t, 15, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timedGrant(x, soon), permanent(ta)}, []string{p.Roles.Approve}, p.Roles.Approve, x, ta)
	if got := permanentAdmin.CapApprovalExpiry(req, x, later); !got.Equal(later) {
		t.Errorf("a name also held through a permanent tenant.admin override was capped to %v", got)
	}
	both := narrowedManager(t, 16, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timedGrant(x, soon), timedGrant(ta, mid)}, []string{p.Roles.Approve}, p.Roles.Approve, x, ta)
	if got := both.CapApprovalExpiry(req, x, later); !got.Equal(mid) {
		t.Errorf("a name held by a grant until %v and a tenant.admin until %v was capped to %v, want the later one", soon, mid, got)
	}
	bothReversed := narrowedManager(t, 17, authority.RankManager, []*role.Role{mgrR}, []membership.GrantedOverride{timedGrant(x, mid), timedGrant(ta, soon)}, []string{p.Roles.Approve}, p.Roles.Approve, x, ta)
	if got := bothReversed.CapApprovalExpiry(req, x, later); !got.Equal(mid) {
		t.Errorf("the direct grant is the later one: capped to %v, want %v", got, mid)
	}
}

// TestHolderRules_SuspendedHoldersStillHoldTheRole: a holder whose person
// is suspended keeps their roles, so a role edit still acts on them and
// the reporting rule still counts them.
func TestHolderRules_SuspendedHoldersStillHoldTheRole(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read)
	c := mkRole(30, tA, "Shared custom", false, 30, false, p.Crm.Leads.Read)
	execRole := mkRole(31, tA, "Exec2", false, 50, false)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgr := actorOf(t, mkMember(t, 15, tA, true, "", mgrR), authority.RankManager, p.Crm.Leads.Read)
	admin := actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	for name, st := range map[string]authority.PersonState{"suspended": suspendedPerson()} {
		peer := mkMemberState(t, 16, st, "", mgrR, c)
		if peer.IsActive() {
			t.Fatalf("%s peer must not count as able to act", name)
		}
		if err := mgr.MayGrantRolePermission(c, p.Crm.Leads.Read, hs(peer)); err == nil {
			t.Errorf("a %s peer manager holds the role: a manager edited it", name)
		}
		boss := mkMember(t, 40, tA, true, "", mgrR)
		holder := mkMemberState(t, 42, st, boss.ID(), execRole)
		if err := admin.MayEditRole(execRole, 20, authority.RoleHolders{Holders: []authority.Member{holder}, Linked: []authority.Member{boss}}); err == nil {
			t.Errorf("a %s holder's reporting line was not checked on a re-level", name)
		}
	}
}

// TestReLevel_TheHoldersOtherLiveRoleDecidesTheirRank: after a re-level a holder's
// rank is the most senior of the new level and their OTHER LIVE roles; a deleted
// role never counts.
func TestReLevel_TheHoldersOtherLiveRoleDecidesTheirRank(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	admin := actorOf(t, mkMember(t, 13, tA, true, "", adminR), authority.RankAdmin, adminEffective()...)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	execRole := mkRole(31, tA, "Exec2", false, 50, false)
	other := mkRole(32, tA, "Other", false, 25, false)
	deleted := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(uid(33)), TenantID: tA, Name: "Gone", HierarchyLevel: 5, IsDeleted: true, CreatedAt: t0})
	mgr := mkMember(t, 40, tA, true, "", mgrR)
	withOther := mkMember(t, 44, tA, true, mgr.ID(), execRole, other)     // rank 25 under a rank-20 manager
	withDeleted := mkMember(t, 45, tA, true, mgr.ID(), execRole, deleted) // rank 50: the deleted role does not count
	h := func(m authority.Member) authority.RoleHolders {
		return authority.RoleHolders{Holders: []authority.Member{m}, Linked: []authority.Member{mgr}}
	}
	cases := []decision{
		{"moving the role senior to the manager breaks a line the other role kept ordered", admin.MayEditRole(execRole, 15, h(withOther)), authority.ErrRoleLevelBreaksReporting},
		{"moving the role junior leaves the other role deciding (25 under 20)", admin.MayEditRole(execRole, 90, h(withOther)), nil},
		{"moving the role to 21 leaves the other role deciding, still junior to the manager", admin.MayEditRole(execRole, 21, h(withOther)), nil},
		{"a deleted role never decides the rank", admin.MayEditRole(execRole, 40, h(withDeleted)), nil},
		{"the same move breaks the line when no role intervenes", admin.MayEditRole(execRole, 20, h(withDeleted)), authority.ErrRoleLevelBreaksReporting},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}
