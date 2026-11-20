package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Hierarchy round 5, commit 1 at the command level: the holders are loaded once
// per command with a fixed number of reads, every active holder's stamp ends in
// the same transaction when the definition of a role changed, and a rename
// touches no holder.

const roleStampReason = "role_definition_changed"

// holdersOf gives a custom role n active holders plus one inactive holder.
func (w *authzWorld) holdersOf(t *testing.T, r *role.Role, n int) {
	t.Helper()
	for range n {
		w.member(t, r, w.exec)
	}
	w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{r.ID()}, Status: membership.StatusInactive})
}

func TestRoleEdits_RotateEveryActiveHolderOnceAndRenameRotatesNone(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	admin := w.adminActor(t)
	r := w.customRole(t, "Shared", 60, p.Crm.Leads.Read)
	w.holdersOf(t, r, 3) // 3 active + 1 inactive holder

	steps := []struct {
		name string
		do   func() error
		want int
	}{
		{"rename only", func() error {
			return h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), Name: "Shared v2", HierarchyLevel: -1, Actor: admin})
		}, 0},
		{"a no-op grant (already held)", func() error {
			return h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: p.Crm.Leads.Read, Actor: admin})
		}, 0},
		{"a real grant", func() error {
			return h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: p.Crm.Leads.ReadAll, Actor: admin})
		}, 3},
		{"a revoke", func() error {
			return h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: p.Crm.Leads.ReadAll, Actor: admin})
		}, 3},
		{"a replace that changes the set", func() error {
			return h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: r.ID(), PermissionNames: []string{p.Crm.Leads.Assign}, Actor: admin})
		}, 3},
		{"a replace with the same set", func() error {
			return h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: r.ID(), PermissionNames: []string{p.Crm.Leads.Assign}, Actor: admin})
		}, 0},
		{"a re-level", func() error {
			return h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 70, Actor: admin})
		}, 3},
		{"a re-level to the same level", func() error {
			return h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 70, Actor: admin})
		}, 0},
		{"a rename together with a re-level", func() error {
			return h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), Name: "Shared v3", HierarchyLevel: 75, Actor: admin})
		}, 3},
	}
	for _, s := range steps {
		before := len(w.rotations)
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		got := w.rotations[before:]
		if len(got) != s.want {
			t.Errorf("%s rotated %d stamps, want %d", s.name, len(got), s.want)
		}
		for _, reason := range got {
			if reason != roleStampReason {
				t.Errorf("%s: rotation reason %q, want %q", s.name, reason, roleStampReason)
			}
		}
	}
}

func TestRoleEdits_RefusedEditsRotateNothingAndChangeNothing(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	r := w.customRole(t, "Shared", 30, p.Crm.Leads.Read)
	peer := w.member(t, w.manager, r)
	w.holdersOf(t, r, 2)
	mgr := actorAs(t, w.member(t, w.manager).ID(), authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll)
	_ = peer

	err := h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: p.Crm.Leads.ReadAll, Actor: mgr})
	if !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("manager edits a role a peer manager holds: %v, want ErrInsufficientRank", err)
	}
	if len(w.rotations) != 0 || len(r.Permissions()) != 1 {
		t.Errorf("a refused edit changed state: %d rotations, %d permissions", len(w.rotations), len(r.Permissions()))
	}
}

func TestRoleEdits_ReLevelIsRefusedWhenItBreaksAReportingLine(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	admin := w.adminActor(t)
	r := w.customRole(t, "Exec2", 50)
	boss := w.member(t, w.manager)
	holder := w.memberReporting(t, boss.ID(), r)
	mate := w.memberReporting(t, holder.ID(), w.customRole(t, "Junior", 60))
	_ = mate

	err := h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 20, Actor: admin})
	if !errors.Is(err, authority.ErrRoleLevelBreaksReporting) {
		t.Fatalf("re-level to the manager's own level: %v, want ErrRoleLevelBreaksReporting", err)
	}
	err = h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 60, Actor: admin})
	if !errors.Is(err, authority.ErrRoleLevelBreaksReporting) {
		t.Fatalf("re-level to the level of the holder's own report: %v, want ErrRoleLevelBreaksReporting", err)
	}
	if r.HierarchyLevel() != 50 || len(w.rotations) != 0 {
		t.Errorf("refused re-levels changed state: level %d, %d rotations", r.HierarchyLevel(), len(w.rotations))
	}
	if err := h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 55, Actor: admin}); err != nil {
		t.Fatalf("re-level that keeps both lines ordered: %v", err)
	}
}

// ----- the cost bound ----------------------------------------------------------

type tallyCounts map[string]int

type tallyMembers struct {
	membership.Repository
	n tallyCounts
}

func (c tallyMembers) ListHoldingRole(ctx context.Context, tid tenant.ID, id role.ID) ([]*membership.Membership, error) {
	c.n["members.ListHoldingRole"]++
	return c.Repository.ListHoldingRole(ctx, tid, id)
}

func (c tallyMembers) ListForTenant(ctx context.Context, tid tenant.ID) ([]*membership.Membership, error) {
	c.n["members.ListForTenant"]++
	return c.Repository.ListForTenant(ctx, tid)
}

func (c tallyMembers) GetByIDs(ctx context.Context, tid tenant.ID, ids []membership.ID) ([]*membership.Membership, error) {
	c.n["members.GetByIDs"]++
	return c.Repository.GetByIDs(ctx, tid, ids)
}

func (c tallyMembers) GetByID(ctx context.Context, tid tenant.ID, id membership.ID) (*membership.Membership, error) {
	c.n["members.GetByID"]++
	return c.Repository.GetByID(ctx, tid, id)
}

type tallyRoles struct {
	role.Repository
	n tallyCounts
}

func (c tallyRoles) GetByIDsIncludingDeleted(ctx context.Context, tid tenant.ID, ids []role.ID) ([]*role.Role, error) {
	c.n["roles.GetByIDsIncludingDeleted"]++
	return c.Repository.GetByIDsIncludingDeleted(ctx, tid, ids)
}

type tallyPersons struct {
	person.Repository
	n tallyCounts
}

func (c tallyPersons) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	c.n["persons.GetByID"]++
	return c.Repository.GetByID(ctx, id)
}

func (c tallyPersons) GetByIDs(ctx context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	c.n["persons.GetByIDs"]++
	return c.Repository.GetByIDs(ctx, ids)
}

func (c tallyPersons) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	c.n["persons.UpdateByID"]++
	return c.Repository.UpdateByID(ctx, id, fn)
}

// TestRoleEdits_HolderReadsDoNotGrowWithTheNumberOfHolders runs the same edit
// over 3 and over 30 holders and compares the repository calls. The stated
// bound: the holders and what the policy needs about them cost a FIXED number of
// calls (one membership list, one role read, one person read; a re-level reads
// the tenant's members once instead of listing holders); the stamp rotation is
// one person write per ACTIVE holder, which the per-person security-stamp event
// requires, and nothing else scales.
func TestRoleEdits_HolderReadsDoNotGrowWithTheNumberOfHolders(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	run := func(holders int, relevel bool) tallyCounts {
		w := newAuthzWorld(t)
		n := tallyCounts{}
		l := command.AuthorityLocker(w.lock)
		pers := tallyPersons{Repository: anyPersons{rotations: &w.rotations}, n: n}
		mem := tallyMembers{Repository: w.members, n: n}
		rol := tallyRoles{Repository: w.roles, n: n}
		admin := w.adminActor(t)
		r := w.customRole(t, "Shared", 60, p.Crm.Leads.Read)
		boss := w.member(t, w.manager)
		for range holders {
			w.memberReporting(t, boss.ID(), r, w.exec)
		}
		w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{r.ID()}, Status: membership.StatusInactive})
		clear(n) // count the command only
		ctx := t.Context()
		if relevel {
			h := command.NewUpdateRoleHandler(fakeUoW{}, l, mem, rol, pers, awClock)
			if err := h.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), HierarchyLevel: 70, Actor: admin}); err != nil {
				t.Fatalf("re-level over %d holders: %v", holders, err)
			}
		} else {
			h := command.NewGrantRolePermissionHandler(fakeUoW{}, l, mem, rol, pers, awClock)
			if err := h.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: p.Crm.Leads.ReadAll, Actor: admin}); err != nil {
				t.Fatalf("grant over %d holders: %v", holders, err)
			}
		}
		if got := n["persons.UpdateByID"]; got != holders {
			t.Errorf("%d holders: %d stamp rotations, want one per active holder", holders, got)
		}
		return n
	}
	for _, relevel := range []bool{false, true} {
		small, large := run(3, relevel), run(30, relevel)
		for _, name := range []string{"members.ListHoldingRole", "members.ListForTenant", "members.GetByID", "roles.GetByIDsIncludingDeleted", "persons.GetByID", "persons.GetByIDs"} {
			if small[name] != large[name] {
				t.Errorf("relevel=%v: %s = %d over 3 holders but %d over 30: it must not grow per holder", relevel, name, small[name], large[name])
			}
		}
		want := map[string]int{"members.ListHoldingRole": 1, "members.ListForTenant": 0, "persons.GetByIDs": 1}
		if relevel {
			want["members.ListHoldingRole"], want["members.ListForTenant"] = 0, 1
		}
		for name, v := range want {
			if large[name] != v {
				t.Errorf("relevel=%v: %s = %d, want %d", relevel, name, large[name], v)
			}
		}
	}
}
