package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The role-side fake lives in internal/identity/domain/role/roletest/
// per TDL Wild Workouts canon — co-located with the aggregate it
// fakes. newFakeRoleRepo is preserved as a one-line alias so existing
// tests don't need rewriting.
func newFakeRoleRepo() *roletest.FakeRepository { return roletest.NewFakeRepository() }

// fakeEdgeRepo is the minimum [rolehierarchy.Repository] surface the
// SetRoleParent + CreateRole-with-parent handlers exercise. Per-tenant
// in-memory map; enforces single-parent invariant (mirroring the
// partial unique index) + multi-hop cycle detection (mirroring the DB
// trigger) so the unit tests cover the same failure modes the adapter
// translates from SQL.
type fakeEdgeRepo struct {
	edges map[rolehierarchy.ID]*rolehierarchy.Edge
}

func newFakeEdgeRepo() *fakeEdgeRepo {
	return &fakeEdgeRepo{edges: make(map[rolehierarchy.ID]*rolehierarchy.Edge)}
}

func (f *fakeEdgeRepo) Add(_ context.Context, e *rolehierarchy.Edge) error {
	// Single-parent invariant — refuse a second active edge for the
	// same child (mirrors uq_role_hierarchy_active_edge_per_child).
	for _, existing := range f.edges {
		if existing.IsActive() && existing.ChildRoleID() == e.ChildRoleID() {
			return rolehierarchy.ErrEdgeAlreadyExists
		}
	}
	// Multi-hop cycle detection — walking child's proposed parent
	// upward, would we ever land back on the child? (mirrors
	// edge_check_cycle trigger).
	if hasCycle(f.edges, e.ChildRoleID(), e.ParentRoleID()) {
		return rolehierarchy.ErrCycle
	}
	f.edges[e.ID()] = e
	return nil
}

func (f *fakeEdgeRepo) GetActiveByChild(_ context.Context, child role.ID) (*rolehierarchy.Edge, error) {
	for _, e := range f.edges {
		if e.IsActive() && e.ChildRoleID() == child {
			return e, nil
		}
	}
	return nil, rolehierarchy.ErrEdgeNotFound
}

func (f *fakeEdgeRepo) UpdateByID(_ context.Context, id rolehierarchy.ID, fn func(*rolehierarchy.Edge) (bool, error)) error {
	e, ok := f.edges[id]
	if !ok {
		return rolehierarchy.ErrEdgeNotFound
	}
	commit, err := fn(e)
	if err != nil {
		return err
	}
	_ = commit
	return nil
}

func (f *fakeEdgeRepo) GetAncestorsByChild(_ context.Context, child role.ID) ([]*rolehierarchy.Edge, error) {
	var out []*rolehierarchy.Edge
	cur := child
	seen := map[role.ID]struct{}{child: {}}
	for {
		var step *rolehierarchy.Edge
		for _, e := range f.edges {
			if e.IsActive() && e.ChildRoleID() == cur {
				step = e
				break
			}
		}
		if step == nil {
			return out, nil
		}
		if _, dup := seen[step.ParentRoleID()]; dup {
			return out, nil
		}
		seen[step.ParentRoleID()] = struct{}{}
		out = append(out, step)
		cur = step.ParentRoleID()
	}
}

func (f *fakeEdgeRepo) ListActiveByParent(_ context.Context, parent role.ID) ([]*rolehierarchy.Edge, error) {
	var out []*rolehierarchy.Edge
	for _, e := range f.edges {
		if e.IsActive() && e.ParentRoleID() == parent {
			out = append(out, e)
		}
	}
	return out, nil
}

var _ rolehierarchy.Repository = (*fakeEdgeRepo)(nil)

// hasCycle reports whether adding edge child→parent would close a
// loop given the existing edge set.
func hasCycle(edges map[rolehierarchy.ID]*rolehierarchy.Edge, child, parent role.ID) bool {
	cur := parent
	seen := map[role.ID]struct{}{child: {}}
	for {
		if _, dup := seen[cur]; dup {
			return true
		}
		seen[cur] = struct{}{}
		var step *rolehierarchy.Edge
		for _, e := range edges {
			if e.IsActive() && e.ChildRoleID() == cur {
				step = e
				break
			}
		}
		if step == nil {
			return false
		}
		cur = step.ParentRoleID()
	}
}

// fakeUoW is a no-op UnitOfWork — calls fn directly without ctx
// propagation. The fake repos don't use TxFromContext, so this is
// sufficient for unit-test coverage. Adapter integration tests are
// the source of truth for real-tx atomicity.
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

// nowFunc returns a stable wall-clock for deterministic tests.
func nowFunc() time.Time {
	return time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
}

// silence unused-imports linter when only some tests reference
// pagination indirectly; helps tighten the import set if it drifts.
var _ = pagination.Cursor{}

// silence unused-imports linter if membership.ID isn't referenced in
// every test.
var _ = membership.ID("")

func newCustomRole(t *testing.T, repo *roletest.FakeRepository, name string) *role.Role {
	t.Helper()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	r, err := role.New(role.ID(ids.NewV7().String()), tid, name, false, 50, false, testNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	r.PullEvents()
	_ = repo.Add(t.Context(), r) // arch-test:ignore-err - test fixture setup
	return r
}

func TestCreateRole_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	out, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		Name:           "Sales Lead",
		HierarchyLevel: 40,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.RoleID.IsZero() {
		t.Error("expected non-empty RoleID")
	}
}

func TestCreateRole_RejectsDuplicateName(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	_ = newCustomRole(t, repo, "Sales Manager") // arch-test:ignore-err - test fixture setup
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		Name:           "Sales Manager",
		HierarchyLevel: 50,
	})
	if !errors.Is(err, command.ErrRoleNameTaken) {
		t.Fatalf("err = %v, want ErrRoleNameTaken", err)
	}
}

func TestUpdateRole_RenameSucceeds(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Old Name")
	h := command.NewUpdateRoleHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdateRoleCommand{
		RoleID:         r.ID(),
		Name:           "New Name",
		HierarchyLevel: -1,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r.Name() != "New Name" {
		t.Errorf("Name = %q, want New Name", r.Name())
	}
}

func TestUpdateRole_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	h := command.NewUpdateRoleHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateRoleCommand{
		RoleID: role.ID("99999999-9999-9999-9999-999999999999"),
		Name:   "x",
	})
	if !errors.Is(err, command.ErrRoleNotFound) {
		t.Fatalf("err = %v, want ErrRoleNotFound", err)
	}
}

func TestReplaceRolePermissions_RejectsUnknown(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Sales Manager")
	h := command.NewReplaceRolePermissionsHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ReplaceRolePermissionsCommand{
		RoleID:          r.ID(),
		PermissionNames: []string{"identity.totally.fake"},
	})
	if !errors.Is(err, command.ErrPermissionUnknown) {
		t.Fatalf("err = %v, want ErrPermissionUnknown", err)
	}
}

func TestGrantRolePermission_AddsPermission(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Sales Manager")
	h := command.NewGrantRolePermissionHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.GrantRolePermissionCommand{
		RoleID:         r.ID(),
		PermissionName: permission.IdentityPermissions.Tenants.View,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(r.Permissions()); got != 1 {
		t.Errorf("Permissions count = %d, want 1", got)
	}
}

func TestRevokeRolePermission_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Sales Manager")
	_ = r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Tenants.View), testNow) // arch-test:ignore-err - test fixture setup
	r.PullEvents()

	h := command.NewRevokeRolePermissionHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RevokeRolePermissionCommand{
		RoleID:         r.ID(),
		PermissionName: permission.IdentityPermissions.Tenants.View,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(r.Permissions()); got != 0 {
		t.Errorf("Permissions count = %d, want 0", got)
	}
}

func TestDeleteRole_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Sales Manager")
	h := command.NewDeleteRoleHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.DeleteRoleCommand{
		RoleID:    r.ID(),
		DeletedBy: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !r.IsDeleted() {
		t.Error("expected role marked deleted")
	}
}

// ----- SetRoleParent (ADR 0058) --------------------------------------------

func TestSetRoleParent_SetsParentOnRootChild(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	edges := newFakeEdgeRepo()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	child := newCustomRole(t, repo, "Junior")
	parent := newCustomRole(t, repo, "Manager")

	h := command.NewSetRoleParentHandler(edges, fakeUoW{}, nowFunc, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID:    tid,
		RoleID:      child.ID(),
		NewParentID: parent.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := edges.GetActiveByChild(t.Context(), child.ID())
	if err != nil {
		t.Fatalf("GetActiveByChild: %v", err)
	}
	if got.ParentRoleID() != parent.ID() {
		t.Errorf("ParentRoleID = %s, want %s", got.ParentRoleID(), parent.ID())
	}
}

func TestSetRoleParent_ReplacesExistingParent(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	edges := newFakeEdgeRepo()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	child := newCustomRole(t, repo, "Junior")
	oldParent := newCustomRole(t, repo, "OldManager")
	newParent := newCustomRole(t, repo, "NewManager")

	h := command.NewSetRoleParentHandler(edges, fakeUoW{}, nowFunc, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	// Seed initial parent.
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID:    tid,
		RoleID:      child.ID(),
		NewParentID: oldParent.ID(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Replace.
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID:    tid,
		RoleID:      child.ID(),
		NewParentID: newParent.ID(),
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := edges.GetActiveByChild(t.Context(), child.ID())
	if err != nil {
		t.Fatalf("GetActiveByChild: %v", err)
	}
	if got.ParentRoleID() != newParent.ID() {
		t.Errorf("ParentRoleID = %s, want %s", got.ParentRoleID(), newParent.ID())
	}
}

func TestSetRoleParent_ClearsParent(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	edges := newFakeEdgeRepo()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	child := newCustomRole(t, repo, "Junior")
	parent := newCustomRole(t, repo, "Manager")

	h := command.NewSetRoleParentHandler(edges, fakeUoW{}, nowFunc, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID: tid, RoleID: child.ID(), NewParentID: parent.ID(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Clear by passing zero parent + reason long enough for the audit floor.
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID: tid,
		RoleID:   child.ID(),
		Reason:   "promotion to root role for org restructure",
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := edges.GetActiveByChild(t.Context(), child.ID()); !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Fatalf("expected no active edge after clear, got: %v", err)
	}
}

func TestSetRoleParent_RejectsSelfReference(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	edges := newFakeEdgeRepo()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	child := newCustomRole(t, repo, "Junior")

	h := command.NewSetRoleParentHandler(edges, fakeUoW{}, nowFunc, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID: tid, RoleID: child.ID(), NewParentID: child.ID(),
	})
	if !errors.Is(err, rolehierarchy.ErrSelfReference) {
		t.Fatalf("err = %v, want ErrSelfReference", err)
	}
}

func TestSetRoleParent_RejectsMultiHopCycle(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	edges := newFakeEdgeRepo()
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	a := newCustomRole(t, repo, "RoleA")
	b := newCustomRole(t, repo, "RoleB")

	h := command.NewSetRoleParentHandler(edges, fakeUoW{}, nowFunc, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	// b → a (legal).
	if err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID: tid, RoleID: b.ID(), NewParentID: a.ID(),
	}); err != nil {
		t.Fatalf("b → a: %v", err)
	}
	// a → b would close the loop.
	err := h.Handle(t.Context(), command.SetRoleParentCommand{
		TenantID: tid, RoleID: a.ID(), NewParentID: b.ID(),
	})
	if !errors.Is(err, rolehierarchy.ErrCycle) {
		t.Fatalf("expected ErrCycle, got: %v", err)
	}
}
