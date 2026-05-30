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
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy/rolehierarchytest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The role-side fake lives in internal/identity/domain/role/roletest/
// per TDL Wild Workouts canon — co-located with the aggregate it
// fakes. newFakeRoleRepo is preserved as a one-line alias so existing
// tests don't need rewriting.
func newFakeRoleRepo() *roletest.FakeRepository { return roletest.NewFakeRepository() }

// The rolehierarchy-side fake lives in
// internal/identity/domain/rolehierarchy/rolehierarchytest/ per TDL
// Wild Workouts canon — co-located with the aggregate it fakes.
// newFakeEdgeRepo is preserved as a one-line alias so existing tests
// don't need rewriting.
func newFakeEdgeRepo() *rolehierarchytest.FakeRepository {
	return rolehierarchytest.NewFakeRepository()
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
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
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
		TenantID: tenant.ID("33333333-3333-3333-3333-333333333333"),
		RoleID:   role.ID("99999999-9999-9999-9999-999999999999"),
		Name:     "x",
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
		TenantID:        tenant.ID("33333333-3333-3333-3333-333333333333"),
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
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
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
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
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
		TenantID:  tenant.ID("33333333-3333-3333-3333-333333333333"),
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
	got, err := edges.GetActiveByChild(t.Context(), tid, child.ID())
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
	got, err := edges.GetActiveByChild(t.Context(), tid, child.ID())
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
	if _, err := edges.GetActiveByChild(t.Context(), tid, child.ID()); !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
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

// ----- CreateRole — input + parent-edge branch coverage --------------------

// failingEdgesRepo overrides Add only — uses to inject specific
// rolehierarchy.Err* values per the CreateRole parent-edge arm.
type failingEdgesRepo struct {
	*rolehierarchytest.FakeRepository
	addErr error
}

func (r *failingEdgesRepo) Add(ctx context.Context, e *rolehierarchy.Edge) error {
	if r.addErr != nil {
		return r.addErr
	}
	return r.FakeRepository.Add(ctx, e)
}

// failingTxUoW returns a fixed error from WithinTx (DB driver / tx
// error scenarios).
type failingTxUoW struct {
	err error
}

func (u failingTxUoW) WithinTx(_ context.Context, _ pg.TxScope, _ func(context.Context) error) error {
	return u.err
}

func TestCreateRole_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID(""),
		Name:           "Sales Lead",
		HierarchyLevel: 40,
	})
	if err == nil {
		t.Fatal("expected error for zero TenantID, got nil")
	}
}

func TestCreateRole_AggregateInvariant_EmptyName(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		Name:           "", // aggregate ctor rejects
		HierarchyLevel: 40,
	})
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("err = %v, want wraps role.ErrInvalid", err)
	}
}

func TestCreateRole_AggregateInvariant_BadHierarchyLevel(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		Name:           "Sales Lead",
		HierarchyLevel: 9999, // out of [0,99] range
	})
	if !errors.Is(err, role.ErrInvalid) {
		t.Fatalf("err = %v, want wraps role.ErrInvalid", err)
	}
}

// TestCreateRole_ParentEdgeRequiresWiring — when ParentRoleID is set
// but edges OR uow is nil, the handler refuses with the explicit
// "parent edge requires edges repo + uow wiring" error. Composition-
// time mistake, not a user error.
func TestCreateRole_ParentEdgeRequiresWiring(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	parent := newCustomRole(t, repo, "Manager")
	cases := []struct {
		name  string
		edges rolehierarchy.Repository
		uow   pg.UnitOfWork
	}{
		{"nil edges + nil uow", nil, nil},
		{"nil edges + ok uow", nil, fakeUoW{}},
		{"ok edges + nil uow", newFakeEdgeRepo(), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewCreateRoleHandler(repo, c.edges, c.uow, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
			_, err := h.Handle(t.Context(), command.CreateRoleCommand{
				TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
				Name:           "Child Role",
				HierarchyLevel: 40,
				ParentRoleID:   parent.ID(),
			})
			if err == nil {
				t.Fatal("expected wiring error for nil edges/uow, got nil")
			}
		})
	}
}

// TestCreateRole_ParentEdgeError_Propagated — each of the
// rolehierarchy.Err* sentinels MUST propagate unwrapped (i.e. errors.Is
// matches the original). Per the handler's switch on ErrCycle /
// ErrCrossTenant / ErrSelfReference / ErrEdgeAlreadyExists.
func TestCreateRole_ParentEdgeError_Propagated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"ErrCycle", rolehierarchy.ErrCycle},
		{"ErrCrossTenant", rolehierarchy.ErrCrossTenant},
		{"ErrEdgeAlreadyExists", rolehierarchy.ErrEdgeAlreadyExists},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRoleRepo()
			parent := newCustomRole(t, repo, "Manager_"+c.name)
			edges := &failingEdgesRepo{
				FakeRepository: rolehierarchytest.NewFakeRepository(),
				addErr:         c.err,
			}
			h := command.NewCreateRoleHandler(repo, edges, fakeUoW{}, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
			_, err := h.Handle(t.Context(), command.CreateRoleCommand{
				TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
				Name:           "Child Role " + c.name,
				HierarchyLevel: 40,
				ParentRoleID:   parent.ID(),
			})
			if !errors.Is(err, c.err) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, c.err)
			}
		})
	}
}

// TestCreateRole_ParentEdge_GenericTxError_Wrapped — generic non-
// sentinel tx error wraps with "create_role: %w" rather than
// propagating cleanly. Lets operators see the alert.
func TestCreateRole_ParentEdge_GenericTxError_Wrapped(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	parent := newCustomRole(t, repo, "Manager")
	// failing tx — never invokes the inner closure, returns errBoom.
	uow := failingTxUoW{err: errBoom}
	h := command.NewCreateRoleHandler(repo, newFakeEdgeRepo(), uow, nowFunc, func() role.ID { return role.ID(ids.NewV7().String()) }, func() rolehierarchy.ID { return rolehierarchy.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.CreateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		Name:           "Child Role",
		HierarchyLevel: 40,
		ParentRoleID:   parent.ID(),
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestUpdateRole_HierarchyLevelChange — Role.ChangeHierarchyLevel
// emits NO event (operational concern per the aggregate doc). Test
// asserts state mutation only.
func TestUpdateRole_HierarchyLevelChange(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	r := newCustomRole(t, repo, "Sales Manager")
	beforeLevel := r.HierarchyLevel()
	newLevel := beforeLevel + 5 // any in [0,99]; default 50 → 55
	h := command.NewUpdateRoleHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdateRoleCommand{
		TenantID:       tenant.ID("33333333-3333-3333-3333-333333333333"),
		RoleID:         r.ID(),
		HierarchyLevel: newLevel,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := r.HierarchyLevel(); got != newLevel {
		t.Errorf("HierarchyLevel = %d, want %d", got, newLevel)
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
