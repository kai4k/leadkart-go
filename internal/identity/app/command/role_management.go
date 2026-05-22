package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrRoleNotFound surfaces when the role ID has no live row in the
// caller's tenant. Soft-deleted roles also return this — repository
// silently filters tombstones from read paths per repository.go.
var ErrRoleNotFound = errors.New("role: not found")

// ErrRoleNameTaken surfaces when role creation collides with an
// existing live role name in the same tenant (DB partial unique
// index 23505 → role.ErrNameTaken → this typed sentinel).
var ErrRoleNameTaken = errors.New("role: name already taken in this tenant")

// ----- CreateRole ----------------------------------------------------------

// CreateRoleCommand carries the validated create-role input. TenantID
// arrives from the caller's JWT — caller doesn't pick a tenant.
//
// IsSuperAdmin is intentionally NOT exposed: the SuperAdmin role is
// seed-only per multi-tenancy.md "SuperUser god-mode". Tenant admins
// cannot promote a custom role to SuperAdmin via HTTP.
//
// ParentRoleID (ADR 0054) — optional parent for the new role. Zero
// value = root (no inheritance). Cross-tenant + cycle prevention runs
// at the DB-trigger layer; the domain accepts it as a constructor-time
// hint (we don't fetch ancestors here because the role doesn't exist
// yet — a fresh role can't be in its own ancestor chain).
type CreateRoleCommand struct {
	TenantID       tenant.ID
	Name           string
	HierarchyLevel int
	ParentRoleID   role.ID
}

// CreateRoleResult returns the new role's ID for the caller.
type CreateRoleResult struct {
	RoleID role.ID
}

// CreateRoleHandler runs the create flow.
type CreateRoleHandler struct {
	roles role.Repository
}

// NewCreateRoleHandler wires the handler.
func NewCreateRoleHandler(r role.Repository) CreateRoleHandler {
	if r == nil {
		panic("command: NewCreateRoleHandler roles repository required")
	}
	return CreateRoleHandler{roles: r}
}

// Handle constructs a custom role + persists it. isSystemDefault +
// isSuperAdmin are always false from this entry point.
//
// ADR 0054 — when ParentRoleID is set, the aggregate's [role.Role.ChangeParent]
// is invoked before Add, using the repo's GetAncestors to detect cycles.
// A fresh role can't appear in its own ancestor chain (it doesn't exist
// yet), so the only failure mode is "parent doesn't exist" / cross-tenant
// parent — surfaced by the DB trigger on Add as ErrHierarchyCrossTenant.
func (h CreateRoleHandler) Handle(ctx context.Context, cmd CreateRoleCommand) (CreateRoleResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateRoleResult{}, errors.New("create_role: tenant id required")
	}
	r, err := role.New(role.ID(ids.NewV7().String()), cmd.TenantID, cmd.Name,
		false /* isSystemDefault */, cmd.HierarchyLevel, false /* isSuperAdmin */)
	if err != nil {
		return CreateRoleResult{}, err
	}
	if !cmd.ParentRoleID.IsZero() {
		// New role can't be a cycle of itself; the lookup closure still
		// has to satisfy the aggregate's non-nil contract.
		if err := r.ChangeParent(cmd.ParentRoleID, func(id role.ID) ([]role.ID, error) {
			ancs, lerr := h.roles.GetAncestors(ctx, id)
			if lerr != nil {
				return nil, lerr
			}
			out := make([]role.ID, 0, len(ancs))
			for _, a := range ancs {
				out = append(out, a.ID())
			}
			return out, nil
		}); err != nil {
			return CreateRoleResult{}, err
		}
	}
	if err := h.roles.Add(ctx, r); err != nil {
		if errors.Is(err, role.ErrNameTaken) {
			return CreateRoleResult{}, ErrRoleNameTaken
		}
		if errors.Is(err, role.ErrHierarchyCycle) ||
			errors.Is(err, role.ErrHierarchyCrossTenant) {
			return CreateRoleResult{}, err
		}
		return CreateRoleResult{}, fmt.Errorf("create_role: %w", err)
	}
	return CreateRoleResult{RoleID: r.ID()}, nil
}

// ----- UpdateRole ----------------------------------------------------------

// UpdateRoleCommand renames and/or re-levels a role.
//
// Both fields are optional in the request DTO; the application maps
// missing fields to "no change". Empty Name skips Rename; zero
// HierarchyLevel uses the sentinel -1 to mean "no change". (We use
// -1 because 0 is a valid valid hierarchy level.)
type UpdateRoleCommand struct {
	RoleID         role.ID
	Name           string // empty = no rename
	HierarchyLevel int    // -1 = no change
}

// UpdateRoleHandler runs the update flow.
type UpdateRoleHandler struct {
	roles role.Repository
}

// NewUpdateRoleHandler wires the handler.
func NewUpdateRoleHandler(r role.Repository) UpdateRoleHandler {
	if r == nil {
		panic("command: NewUpdateRoleHandler roles repository required")
	}
	return UpdateRoleHandler{roles: r}
}

// Handle dispatches to the aggregate. System-default roles +
// SuperAdmin reject mutation per the aggregate's ensureMutable gate.
func (h UpdateRoleHandler) Handle(ctx context.Context, cmd UpdateRoleCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("update_role: role id required")
	}
	err := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		mutated := false
		if cmd.Name != "" {
			if err := r.Rename(cmd.Name); err != nil {
				return false, err
			}
			mutated = true
		}
		if cmd.HierarchyLevel >= 0 {
			if err := r.ChangeHierarchyLevel(cmd.HierarchyLevel); err != nil {
				return false, err
			}
			mutated = true
		}
		return mutated, nil
	})
	switch {
	case errors.Is(err, role.ErrNotFound):
		return ErrRoleNotFound
	case errors.Is(err, role.ErrNameTaken):
		return ErrRoleNameTaken
	case err != nil:
		return fmt.Errorf("update_role: %w", err)
	}
	return nil
}

// ----- ReplaceRolePermissions ----------------------------------------------

// ReplaceRolePermissionsCommand atomically replaces the role's
// permission set. Names MUST be catalogue-known; unknown names →
// ErrPermissionUnknown.
type ReplaceRolePermissionsCommand struct {
	RoleID          role.ID
	PermissionNames []string
}

// ReplaceRolePermissionsHandler runs the replace flow.
type ReplaceRolePermissionsHandler struct {
	roles role.Repository
}

// NewReplaceRolePermissionsHandler wires the handler.
func NewReplaceRolePermissionsHandler(r role.Repository) ReplaceRolePermissionsHandler {
	if r == nil {
		panic("command: NewReplaceRolePermissionsHandler roles repository required")
	}
	return ReplaceRolePermissionsHandler{roles: r}
}

// Handle resolves names + dispatches to [Role.ReplacePermissions].
func (h ReplaceRolePermissionsHandler) Handle(ctx context.Context, cmd ReplaceRolePermissionsCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("replace_role_permissions: role id required")
	}
	target, err := resolvePermissions(cmd.PermissionNames)
	if err != nil {
		return err
	}
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.ReplacePermissions(target); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(upErr, role.ErrNotFound) {
		return ErrRoleNotFound
	}
	if upErr != nil {
		return fmt.Errorf("replace_role_permissions: %w", upErr)
	}
	return nil
}

// ----- GrantRolePermission --------------------------------------------------

// GrantRolePermissionCommand adds a single permission to the role.
type GrantRolePermissionCommand struct {
	RoleID         role.ID
	PermissionName string
}

// GrantRolePermissionHandler runs the grant flow.
type GrantRolePermissionHandler struct {
	roles role.Repository
}

// NewGrantRolePermissionHandler wires the handler.
func NewGrantRolePermissionHandler(r role.Repository) GrantRolePermissionHandler {
	if r == nil {
		panic("command: NewGrantRolePermissionHandler roles repository required")
	}
	return GrantRolePermissionHandler{roles: r}
}

// Handle dispatches to [Role.GrantPermission].
func (h GrantRolePermissionHandler) Handle(ctx context.Context, cmd GrantRolePermissionCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("grant_role_permission: role id required")
	}
	p, err := permission.TryFromConstant(cmd.PermissionName)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrPermissionUnknown, cmd.PermissionName)
	}
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.GrantPermission(p); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(upErr, role.ErrNotFound) {
		return ErrRoleNotFound
	}
	if upErr != nil {
		return fmt.Errorf("grant_role_permission: %w", upErr)
	}
	return nil
}

// ----- RevokeRolePermission -------------------------------------------------

// RevokeRolePermissionCommand removes a single permission.
type RevokeRolePermissionCommand struct {
	RoleID         role.ID
	PermissionName string
}

// RevokeRolePermissionHandler runs the revoke flow.
type RevokeRolePermissionHandler struct {
	roles role.Repository
}

// NewRevokeRolePermissionHandler wires the handler.
func NewRevokeRolePermissionHandler(r role.Repository) RevokeRolePermissionHandler {
	if r == nil {
		panic("command: NewRevokeRolePermissionHandler roles repository required")
	}
	return RevokeRolePermissionHandler{roles: r}
}

// Handle dispatches to [Role.RevokePermission].
func (h RevokeRolePermissionHandler) Handle(ctx context.Context, cmd RevokeRolePermissionCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("revoke_role_permission: role id required")
	}
	p, err := permission.TryFromConstant(cmd.PermissionName)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrPermissionUnknown, cmd.PermissionName)
	}
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.RevokePermission(p); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(upErr, role.ErrNotFound) {
		return ErrRoleNotFound
	}
	if upErr != nil {
		return fmt.Errorf("revoke_role_permission: %w", upErr)
	}
	return nil
}

// ----- DeleteRole ----------------------------------------------------------

// DeleteRoleCommand soft-deletes a role. DeletedBy is the caller's
// PersonID (from the JWT Subject claim) — recorded for audit.
//
// System-default roles + SuperAdmin reject deletion via the
// aggregate's ensureMutable + Delete checks.
type DeleteRoleCommand struct {
	RoleID    role.ID
	DeletedBy string
}

// DeleteRoleHandler runs the delete flow.
type DeleteRoleHandler struct {
	roles role.Repository
}

// NewDeleteRoleHandler wires the handler.
func NewDeleteRoleHandler(r role.Repository) DeleteRoleHandler {
	if r == nil {
		panic("command: NewDeleteRoleHandler roles repository required")
	}
	return DeleteRoleHandler{roles: r}
}

// Handle dispatches to [Role.Delete].
func (h DeleteRoleHandler) Handle(ctx context.Context, cmd DeleteRoleCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("delete_role: role id required")
	}
	err := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.Delete(cmd.DeletedBy); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, role.ErrNotFound) {
		return ErrRoleNotFound
	}
	if err != nil {
		return fmt.Errorf("delete_role: %w", err)
	}
	return nil
}

// ----- SetRoleParent (ADR 0054) ---------------------------------------------

// SetRoleParentCommand carries the validated set-parent input.
//
// NewParentID == zero clears the parent (role becomes a root). The
// handler runs in-memory cycle detection via GetAncestors before the
// aggregate mutation; the DB trigger is the final strict gate.
type SetRoleParentCommand struct {
	RoleID      role.ID
	NewParentID role.ID
}

// SetRoleParentHandler runs the set-parent flow.
type SetRoleParentHandler struct {
	roles role.Repository
}

// NewSetRoleParentHandler wires the handler.
func NewSetRoleParentHandler(r role.Repository) SetRoleParentHandler {
	if r == nil {
		panic("command: NewSetRoleParentHandler roles repository required")
	}
	return SetRoleParentHandler{roles: r}
}

// Handle dispatches to [Role.ChangeParent] inside the repo's UpdateByID
// transaction. Pre-validates the parent's ancestor chain so the best
// ergonomic error message wins; the DB trigger is the strict-gate
// fallback per ADR 0054.
func (h SetRoleParentHandler) Handle(ctx context.Context, cmd SetRoleParentCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("set_role_parent: role id required")
	}
	err := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.ChangeParent(cmd.NewParentID, func(id role.ID) ([]role.ID, error) {
			ancs, lerr := h.roles.GetAncestors(ctx, id)
			if lerr != nil {
				return nil, lerr
			}
			out := make([]role.ID, 0, len(ancs))
			for _, a := range ancs {
				out = append(out, a.ID())
			}
			return out, nil
		}); err != nil {
			return false, err
		}
		return true, nil
	})
	switch {
	case errors.Is(err, role.ErrNotFound):
		return ErrRoleNotFound
	case errors.Is(err, role.ErrHierarchyCycle),
		errors.Is(err, role.ErrHierarchyCrossTenant):
		return err
	case err != nil:
		return fmt.Errorf("set_role_parent: %w", err)
	}
	return nil
}
