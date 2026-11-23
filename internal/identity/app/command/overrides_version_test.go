package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// formVersion is the overrides version a form opened at `at` holds for m: what
// GET /users/{id} hands a caller who may change them (the live set at `at`).
func (w *authzWorld) formVersion(t *testing.T, m *membership.Membership, at time.Time) string {
	t.Helper()
	var roles []*role.Role
	if ids := m.RoleAssignments(); len(ids) > 0 {
		var err error
		if roles, err = w.roles.GetByIDsIncludingDeleted(t.Context(), awTenant, ids); err != nil {
			t.Fatalf("form version: load roles: %v", err)
		}
	}
	member, err := authority.MemberOf(m, roles, at, authority.NewPersonState(true, false))
	if err != nil {
		t.Fatalf("form version: %v", err)
	}
	return member.Overrides().Version()
}

// ver is [authzWorld.formVersion] at the handlers' own clock: a form opened
// the instant it is saved.
func (w *authzWorld) ver(t *testing.T, m *membership.Membership) string {
	t.Helper()
	return w.formVersion(t, m, awClock())
}

// A save carries the version of the overrides its form was opened on. When the
// live overrides are no longer that state, the save is refused and nothing is
// stored: the two ways the review found a stale form silently changing access.
func TestReplaceOverrides_AStaleFormChangesNothing(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x, y := p.Crm.Leads.Assign, p.Inventory.Catalog.Manage
	type world struct {
		w     *authzWorld
		exec  *membership.Membership
		owner authority.Actor
	}
	setup := func(t *testing.T, grants ...membership.GrantedOverride) world {
		t.Helper()
		w := newAuthzWorld(t)
		exec := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()}, GrantedPermissions: grants})
		ownerM := w.member(t, w.owner)
		// an owner holds every name, so no ceiling refuses these saves: only the precondition can
		return world{w: w, exec: exec, owner: actorAs(t, ownerM.ID(), authority.RankOwner, tenantAdminEffective()...)}
	}
	later := func() time.Time { return testNow.Add(2 * time.Hour) }

	t.Run("a grant that expired after the form opened is not stored permanent", func(t *testing.T) {
		t.Parallel()
		expires := testNow.Add(time.Hour)
		s := setup(t, membership.GrantedOverride{Permission: permission.FromConstant(x), ExpiresAt: expires})
		opened := s.w.formVersion(t, s.exec, testNow) // the grant is live: the form lists it
		err := s.w.handlersAt(later).override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
			TenantID: awTenant, MembershipID: s.exec.ID(), GrantedNames: []string{x}, OverridesVersion: opened, Actor: s.owner})
		if !errors.Is(err, command.ErrOverridesChanged) {
			t.Fatalf("err = %v, want ErrOverridesChanged (the grant expired between the read and the save)", err)
		}
		got := s.exec.GrantedPermissions()
		if len(got) != 1 || !got[0].ExpiresAt.Equal(expires) {
			t.Fatalf("grants after the refused save = %+v, want the expired grant untouched (never permanent)", got)
		}
		if len(s.w.rotations) != 0 {
			t.Errorf("a refused save rotated the member's stamp: %v", s.w.rotations)
		}
	})

	t.Run("an approval that landed after the form opened is not erased", func(t *testing.T) {
		t.Parallel()
		s := setup(t)
		opened := s.w.ver(t, s.exec) // no overrides yet
		expires := testNow.Add(72 * time.Hour)
		if err := s.w.members.UpdateByID(t.Context(), awTenant, s.exec.ID(), func(m *membership.Membership) (bool, error) {
			return true, m.GrantPermission(permission.FromConstant(y), expires, testNow) // what an approval writes
		}); err != nil {
			t.Fatalf("approve: %v", err)
		}
		err := s.w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
			TenantID: awTenant, MembershipID: s.exec.ID(), GrantedNames: []string{x}, OverridesVersion: opened, Actor: s.owner})
		if !errors.Is(err, command.ErrOverridesChanged) {
			t.Fatalf("err = %v, want ErrOverridesChanged (a grant was approved after the read)", err)
		}
		got := s.exec.GrantedPermissions()
		if len(got) != 1 || got[0].Permission.Name() != y || !got[0].ExpiresAt.Equal(expires) {
			t.Fatalf("grants after the refused save = %+v, want only the approved %s until %v", got, y, expires)
		}
	})

	t.Run("a revocation saved by someone else is not undone", func(t *testing.T) {
		t.Parallel()
		s := setup(t)
		opened := s.w.ver(t, s.exec)
		h := s.w.handlers().override
		if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: s.exec.ID(),
			RevokedNames: []string{p.Crm.Leads.Read}, OverridesVersion: opened, Actor: s.owner}); err != nil {
			t.Fatalf("the first save: %v", err)
		}
		err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: s.exec.ID(),
			GrantedNames: []string{x}, OverridesVersion: opened, Actor: s.owner})
		if !errors.Is(err, command.ErrOverridesChanged) {
			t.Fatalf("a second save from the same form: err = %v, want ErrOverridesChanged", err)
		}
		if got := s.exec.RevokedPermissions(); len(got) != 1 || got[0].Name() != p.Crm.Leads.Read {
			t.Fatalf("revoked after the refused save = %v, want the first save's revocation kept", got)
		}
	})

	t.Run("a save without a version is refused, on an empty set too", func(t *testing.T) {
		t.Parallel()
		s := setup(t)
		for _, v := range []string{"", " ", "ov1-", authority.Overrides{Revoked: []string{x}}.Version()} {
			err := s.w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
				TenantID: awTenant, MembershipID: s.exec.ID(), GrantedNames: []string{x}, OverridesVersion: v, Actor: s.owner})
			if !errors.Is(err, command.ErrOverridesChanged) {
				t.Errorf("version %q: err = %v, want ErrOverridesChanged", v, err)
			}
		}
		if got := s.exec.GrantedPermissions(); len(got) != 0 {
			t.Fatalf("grants = %+v, want none stored", got)
		}
	})

	t.Run("the matching version saves, and the next save needs the new one", func(t *testing.T) {
		t.Parallel()
		s := setup(t)
		h := s.w.handlers().override
		first := s.w.ver(t, s.exec)
		if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: s.exec.ID(),
			GrantedNames: []string{x}, OverridesVersion: first, Actor: s.owner}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if got := s.exec.GrantedPermissions(); len(got) != 1 || got[0].Permission.Name() != x {
			t.Fatalf("grants = %+v, want %s", got, x)
		}
		second := s.w.ver(t, s.exec)
		if second == first {
			t.Fatal("the version did not change with the overrides")
		}
		if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: s.exec.ID(),
			GrantedNames: []string{x, y}, OverridesVersion: second, Actor: s.owner}); err != nil {
			t.Fatalf("save from a fresh read: %v", err)
		}
	})

	t.Run("a live time-bound grant re-sent with its version keeps its expiry", func(t *testing.T) {
		t.Parallel()
		expires := testNow.Add(time.Hour)
		s := setup(t, membership.GrantedOverride{Permission: permission.FromConstant(x), ExpiresAt: expires})
		if err := s.w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: s.exec.ID(),
			GrantedNames: []string{x, y}, OverridesVersion: s.w.ver(t, s.exec), Actor: s.owner}); err != nil {
			t.Fatalf("save: %v", err)
		}
		for _, g := range s.exec.GrantedPermissions() {
			if g.Permission.Name() == x && !g.ExpiresAt.Equal(expires) {
				t.Errorf("%s expires %v after the save, want its original %v", x, g.ExpiresAt, expires)
			}
		}
	})
}

// Authority is decided before the precondition: a caller who may not act on the
// member is refused by rank whatever version they send, so the answer never
// tells them whether a guessed version matched. A caller who may act on the
// member but sends a stale version is told the form is stale before the ceiling
// is consulted (the stale form's names are not what they meant to grant).
func TestReplaceOverrides_RankIsDecidedBeforeTheVersion(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	w := newAuthzWorld(t)
	exec := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()},
		GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: testNow.Add(time.Hour)}}})
	peerM := w.member(t, w.exec)
	peer := actorAs(t, peerM.ID(), authority.RankUser, p.Crm.Leads.Read)
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read) // may act on exec; does not hold X
	h := w.handlers().override
	for _, v := range []string{"", "stale", w.ver(t, exec)} {
		err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
			GrantedNames: []string{x}, OverridesVersion: v, Actor: peer})
		if !errors.Is(err, authority.ErrInsufficientRank) || errors.Is(err, command.ErrOverridesChanged) {
			t.Errorf("a peer with version %q: err = %v, want ErrInsufficientRank and never ErrOverridesChanged", v, err)
		}
	}
	// the manager's form predates the expiry: stale, so the conflict, not the ceiling
	err := w.handlersAt(func() time.Time { return testNow.Add(2 * time.Hour) }).override.Handle(t.Context(),
		command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
			GrantedNames: []string{x}, OverridesVersion: w.ver(t, exec), Actor: mgr})
	if !errors.Is(err, command.ErrOverridesChanged) || errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("a senior with a stale form: err = %v, want ErrOverridesChanged", err)
	}
}
