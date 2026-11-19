package authority_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// A name listed more than once with different expiries is a change to that
// name whichever order the entries come in (the matching expiry listed LAST
// must not hide the conflicting one).
func TestOverrides_DuplicateNamesWithDifferentExpiriesAreAChangeInEitherOrder(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	aMgr := actorOf(t, mkMember(t, 15, tA, true, "", mgrR), authority.RankManager, p.Crm.Leads.Read)
	week := t0.Add(7 * 24 * time.Hour)
	timed := mkMemberOv(t, 18, tA, []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: week}}, nil, userR)
	perm := mkMemberOv(t, 21, tA, []membership.GrantedOverride{permanent(x)}, nil, userR)
	wk := authority.Override{Name: x, ExpiresAt: week}
	pm := authority.Override{Name: x}
	cases := []decision{
		{"timed before, matching expiry first", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{wk, pm}}), authority.ErrGrantExceedsAuthority},
		{"timed before, matching expiry last", aMgr.MayChangeOverrides(timed, authority.Overrides{Granted: []authority.Override{pm, wk}}), authority.ErrGrantExceedsAuthority},
		{"permanent before, matching expiry first", aMgr.MayChangeOverrides(perm, authority.Overrides{Granted: []authority.Override{pm, wk}}), authority.ErrGrantExceedsAuthority},
		{"permanent before, matching expiry last", aMgr.MayChangeOverrides(perm, authority.Overrides{Granted: []authority.Override{wk, pm}}), authority.ErrGrantExceedsAuthority},
		{"identical duplicates are no change", aMgr.MayChangeOverrides(perm, authority.Overrides{Granted: []authority.Override{pm, pm}}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// The snapshot a Member hands out is a copy of BOTH lists: mutating what
// Overrides() returned must not change a later decision.
func TestOverrides_ReturnedSlicesAreCopies(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	aMgr := actorOf(t, mkMember(t, 15, tA, true, "", mgrR), authority.RankManager, p.Crm.Leads.Read)
	target := mkMemberOv(t, 18, tA, []membership.GrantedOverride{permanent(p.Crm.Leads.Read)}, []string{p.Crm.Leads.Assign}, userR)

	snap := target.Overrides()
	snap.Revoked[0] = p.Crm.Leads.Read // forge: pretend Assign was never revoked
	snap.Granted[0].Name = p.Crm.Leads.Read
	snap.Revoked = snap.Revoked[:0]
	// Un-revoking Assign (a name the actor does not hold) must still hit the ceiling.
	err := aMgr.MayChangeOverrides(target, authority.Overrides{Granted: grants(p.Crm.Leads.Read)})
	if !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("un-revoke after mutating the returned snapshot: %v, want ErrGrantExceedsAuthority", err)
	}
	if got := target.Overrides(); !slices.Equal(got.Revoked, []string{p.Crm.Leads.Assign}) {
		t.Errorf("Revoked changed through the returned slice: %v", got.Revoked)
	}
}

// A zero decision instant cannot separate live from expired grants, so the
// projection refuses to exist.
func TestMemberOf_ZeroNowIsRefused(t *testing.T) {
	t.Parallel()
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(18)), PersonID: person.ID(uid(518)),
		TenantID: tA, Status: membership.StatusActive, JoinedAt: t0, RoleAssignments: []role.ID{userR.ID()}})
	got, err := authority.MemberOf(m, []*role.Role{userR}, time.Time{}, activePerson())
	if !errors.Is(err, authority.ErrNoAuthority) || got.Valid() {
		t.Fatalf("MemberOf with zero now: valid=%v err=%v, want ErrNoAuthority and an invalid Member", got.Valid(), err)
	}
}
