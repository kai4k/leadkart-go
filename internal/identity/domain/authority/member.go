package authority

import (
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Member is the authority-relevant projection of a membership (ADR 0069).
// Build it with MemberOf; the zero Member is invalid and every decision that
// receives one denies.
type Member struct {
	valid      bool
	id         membership.ID
	tenantID   tenant.ID
	rank       Rank
	owner      bool
	superAdmin bool
	active     bool
	reportsTo  membership.ID
	overrides  Overrides
	// statusActive is the membership's own status (active), without the person
	// state `active` also folds in: a locked or suspended person still holds
	// their roles, so a role edit acts on them.
	statusActive bool
	// levels is the effective level of every live role the member holds, by role
	// id: the input of "what would this member's rank be if role X moved to L".
	levels map[role.ID]Rank
}

// PersonState is whether the person behind a membership can act at all. The
// zero value means "not known" and counts as cannot act, so a loader that
// forgets the person gets the safe answer. Build it with NewPersonState from
// the person's facts, never from a bare bool.
type PersonState struct{ canAct bool }

// NewPersonState derives the state from the person's facts: active (not
// deactivated), globally suspended. The person can act only when
// active and not suspended.
//
// A LOGIN LOCK (failed-login throttle) is deliberately not an input: it exists
// to slow password guessing at the sign-in door and says nothing about a session
// that is already authenticated. Counting it here would let anyone who knows an
// administrator's email lock them out of administering by failing logins as them
// (ADR 0069, "Pending owner confirmation": a login lock does not suspend
// authority). Every reader of the state follows: a locked member acts, is an
// active member of a tenant, is an eligible manager, counts as an owner for the
// last-owner guard and as a holder.
func NewPersonState(active, globallySuspended bool) PersonState {
	return PersonState{canAct: active && !globallySuspended}
}

// CanAct reports whether the person can act; false for the zero PersonState.
func (p PersonState) CanAct() bool { return p.canAct }

// MemberOf projects m with every role it holds. roles must be exactly the
// roles named by m.RoleAssignments(), deleted ones included (they are ignored
// for rank and flags but must be present, so a partial load cannot make an
// owner look like the weakest member), and every role must belong to m's
// tenant. Any mismatch, or an id that does not canonicalise, is ErrNoAuthority.
//
// now is the instant the caller decides at (the domain never reads the clock):
// it separates the membership's live permission overrides from expired ones.
// person says whether the person behind m can act at all (see PersonState): a
// member whose person cannot act, or whose state is unknown, is not active, so
// it never counts as "another active owner" (GuardLastOwner) and is never an
// eligible manager. The member carries a snapshot of m's live overrides, which
// is the "before" of every MayChangeOverrides decision.
func MemberOf(m *membership.Membership, roles []*role.Role, now time.Time, person PersonState) (Member, error) {
	if m == nil {
		return Member{}, fmt.Errorf("%w: membership required", ErrNoAuthority)
	}
	if now.IsZero() {
		return Member{}, fmt.Errorf("%w: the decision instant is required", ErrNoAuthority)
	}
	id, err := membership.ParseID(string(m.ID()))
	if err != nil {
		return Member{}, fmt.Errorf("%w: membership id is not a canonical uuid", ErrNoAuthority)
	}
	tid, err := tenant.ParseID(string(m.TenantID()))
	if err != nil {
		return Member{}, fmt.Errorf("%w: membership tenant id is not a canonical uuid", ErrNoAuthority)
	}
	var reportsTo membership.ID
	if m.ReportsTo() != "" {
		if reportsTo, err = membership.ParseID(string(m.ReportsTo())); err != nil {
			return Member{}, fmt.Errorf("%w: manager id is not a canonical uuid", ErrNoAuthority)
		}
	}
	if err := checkRolesComplete(m, tid, roles); err != nil {
		return Member{}, err
	}
	out := Member{valid: true, id: id, tenantID: tid, rank: RankOf(roles),
		active: person.CanAct() && m.Status() == membership.StatusActive, reportsTo: reportsTo,
		overrides: liveOverrides(m, now), statusActive: m.Status() == membership.StatusActive,
		levels: make(map[role.ID]Rank, len(roles))}
	for _, r := range roles {
		if r.IsDeleted() {
			continue
		}
		out.levels[r.ID()] = levelOf(r)
		out.owner = out.owner || r.IsCompanyOwner()
		out.superAdmin = out.superAdmin || r.IsSuperAdmin()
	}
	return out, nil
}

// liveOverrides snapshots m's overrides at now: grants not yet expired (the
// same test the permission resolver applies) with their expiry, and the
// revoked names.
func liveOverrides(m *membership.Membership, now time.Time) Overrides {
	out := Overrides{Granted: []Override{}, Revoked: []string{}}
	for _, g := range m.GrantedPermissions() {
		if g.Permission == nil || (!g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)) {
			continue
		}
		out.Granted = append(out.Granted, Override{Name: g.Permission.Name(), ExpiresAt: g.ExpiresAt})
	}
	for _, p := range m.RevokedPermissions() {
		if p != nil {
			out.Revoked = append(out.Revoked, p.Name())
		}
	}
	return out
}

// checkRolesComplete: the loaded role ids equal m.RoleAssignments() as sets
// and each role is in tenant tid.
func checkRolesComplete(m *membership.Membership, tid tenant.ID, roles []*role.Role) error {
	want := make(map[role.ID]struct{}, len(m.RoleAssignments()))
	for _, rid := range m.RoleAssignments() {
		c, err := role.ParseID(string(rid))
		if err != nil {
			return fmt.Errorf("%w: assigned role id is not a canonical uuid", ErrNoAuthority)
		}
		want[c] = struct{}{}
	}
	got := make(map[role.ID]struct{}, len(roles))
	for _, r := range roles {
		if r == nil {
			return fmt.Errorf("%w: nil role in the loaded set", ErrNoAuthority)
		}
		c, err := role.ParseID(string(r.ID()))
		if err != nil {
			return fmt.Errorf("%w: loaded role id is not a canonical uuid", ErrNoAuthority)
		}
		if !sameTenant(r.TenantID(), tid) {
			return fmt.Errorf("%w: role %s belongs to another tenant", ErrNoAuthority, c)
		}
		if _, ok := want[c]; !ok {
			return fmt.Errorf("%w: role %s is not assigned to the membership", ErrNoAuthority, c)
		}
		got[c] = struct{}{}
	}
	if len(got) != len(want) {
		return fmt.Errorf("%w: %d of %d assigned roles loaded; the full set is required", ErrNoAuthority, len(got), len(want))
	}
	return nil
}

// sameTenant compares two tenant ids in canonical form; an id that does not
// canonicalise is never equal to anything.
func sameTenant(a, b tenant.ID) bool {
	ca, errA := ids.Canonical(string(a))
	cb, errB := ids.Canonical(string(b))
	return errA == nil && errB == nil && ca == cb
}

// Valid reports whether the Member was built by MemberOf.
func (m Member) Valid() bool { return m.valid }

// ID is the membership id (canonical).
func (m Member) ID() membership.ID { return m.id }

// TenantID is the membership's tenant (canonical).
func (m Member) TenantID() tenant.ID { return m.tenantID }

// Rank is the most senior live role level (RankNone when none, and for the zero Member).
func (m Member) Rank() Rank {
	if !m.valid {
		return RankNone
	}
	return m.rank
}

// IsOwner reports whether the member holds the CompanyOwner role.
func (m Member) IsOwner() bool { return m.valid && m.owner }

// IsSuperAdmin reports whether the member holds a super-admin role.
func (m Member) IsSuperAdmin() bool { return m.valid && m.superAdmin }

// rankIfRoleMovedTo is the member's rank were the role rid at level l: the most
// senior of l and the levels of every other live role the member holds
// (RankNone when there is none). A member who does not hold rid keeps their rank.
func (m Member) rankIfRoleMovedTo(rid role.ID, l Rank) Rank {
	if _, holds := m.levels[rid]; !holds {
		return m.rank
	}
	best := RankNone
	for id, lvl := range m.levels {
		if id == rid {
			lvl = l
		}
		best = min(best, lvl)
	}
	return best
}

// IsActive reports whether the membership is active AND the person behind it can act.
func (m Member) IsActive() bool { return m.valid && m.active }

// Overrides is a copy of the member's live permission overrides as loaded by
// MemberOf (empty for the zero Member).
func (m Member) Overrides() Overrides {
	return Overrides{Granted: slices.Clone(m.overrides.Granted), Revoked: slices.Clone(m.overrides.Revoked)}
}

// ReportsTo is the member's current manager (zero when none).
func (m Member) ReportsTo() membership.ID { return m.reportsTo }
