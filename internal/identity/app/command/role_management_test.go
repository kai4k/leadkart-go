package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeRoleRepo is the minimum [role.Repository] surface the role
// management handlers exercise.
type fakeRoleRepo struct {
	roles map[role.ID]*role.Role
	names map[string]role.ID // (tenant_id|name) → id, for ErrNameTaken
}

func newFakeRoleRepo() *fakeRoleRepo {
	return &fakeRoleRepo{
		roles: make(map[role.ID]*role.Role),
		names: make(map[string]role.ID),
	}
}

func nameKey(tid tenant.ID, name string) string { return tid.String() + "|" + name }

func (r *fakeRoleRepo) Add(_ context.Context, x *role.Role) error {
	if _, ok := r.names[nameKey(x.TenantID(), x.Name())]; ok {
		return role.ErrNameTaken
	}
	r.roles[x.ID()] = x
	r.names[nameKey(x.TenantID(), x.Name())] = x.ID()
	return nil
}

func (r *fakeRoleRepo) UpdateByID(_ context.Context, id role.ID, fn func(*role.Role) (bool, error)) error {
	x, ok := r.roles[id]
	if !ok {
		return role.ErrNotFound
	}
	commit, err := fn(x)
	if err != nil {
		return err
	}
	_ = commit
	return nil
}

func (r *fakeRoleRepo) GetByID(_ context.Context, id role.ID) (*role.Role, error) {
	x, ok := r.roles[id]
	if !ok {
		return nil, role.ErrNotFound
	}
	return x, nil
}

func (r *fakeRoleRepo) GetByTenantAndName(_ context.Context, tid tenant.ID, name string) (*role.Role, error) {
	id, ok := r.names[nameKey(tid, name)]
	if !ok {
		return nil, role.ErrNotFound
	}
	return r.roles[id], nil
}

func (r *fakeRoleRepo) GetByIDs(_ context.Context, ids []role.ID) ([]*role.Role, error) {
	var out []*role.Role
	for _, id := range ids {
		if x, ok := r.roles[id]; ok {
			out = append(out, x)
		}
	}
	return out, nil
}

func (r *fakeRoleRepo) ListByTenant(_ context.Context, tid tenant.ID) ([]*role.Role, error) {
	var out []*role.Role
	for _, x := range r.roles {
		if x.TenantID() == tid {
			out = append(out, x)
		}
	}
	return out, nil
}

var _ role.Repository = (*fakeRoleRepo)(nil)

func newCustomRole(t *testing.T, repo *fakeRoleRepo, name string) *role.Role {
	t.Helper()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
	tid := tenant.ID("33333333-3333-3333-3333-333333333333")
	r, err := role.New(role.ID(ids.NewV7().String()), tid, name, false, 50, false)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	r.PullEvents()
	_ = repo.Add(t.Context(), r)
	return r
}

func TestCreateRole_Succeeds(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
	repo := newFakeRoleRepo()
	h := command.NewCreateRoleHandler(repo)
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
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
	repo := newFakeRoleRepo()
	_ = newCustomRole(t, repo, "Sales Manager")
	h := command.NewCreateRoleHandler(repo)
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
	h := command.NewUpdateRoleHandler(repo)
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
	h := command.NewUpdateRoleHandler(repo)
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
	h := command.NewReplaceRolePermissionsHandler(repo)
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
	h := command.NewGrantRolePermissionHandler(repo)
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
	_ = r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Tenants.View))
	r.PullEvents()

	h := command.NewRevokeRolePermissionHandler(repo)
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
	h := command.NewDeleteRoleHandler(repo)
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
