package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
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
// ParentRoleID (ADR 0058 — Wave 9.4) — optional parent for the new
// role. Zero value = root (no edge created). When set, the handler
// creates the Role + the rolehierarchy.Edge in one UoW transaction.
// Cross-tenant + cycle prevention runs declaratively at the DB layer
// (composite FK + cycle trigger).
//
// ActorMembershipID is injected from the JWT — recorded on the edge
// for audit. Zero is tolerated (system / bootstrap paths).
type CreateRoleCommand struct {
	TenantID          tenant.ID
	Name              string
	HierarchyLevel    int
	ParentRoleID      role.ID
	ActorMembershipID membership.ID
	Reason            string
}

// CreateRoleResult returns the new role's ID for the caller.
type CreateRoleResult struct {
	RoleID role.ID
}

// CreateRoleHandler runs the create flow.
type CreateRoleHandler struct {
	roles role.Repository
	edges rolehierarchy.Repository
	uow   pg.UnitOfWork
	now   func() time.Time
}

// NewCreateRoleHandler wires the handler. `edges` may be nil when the
// caller knows ParentRoleID will always be zero (test fixtures); the
// handler refuses to seed an edge in that case.
func NewCreateRoleHandler(r role.Repository, edges rolehierarchy.Repository, uow pg.UnitOfWork, now func() time.Time) CreateRoleHandler {
	if r == nil {
		panic("command: NewCreateRoleHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateRoleHandler{roles: r, edges: edges, uow: uow, now: now}
}

// Handle constructs a custom role + persists it. isSystemDefault +
// isSuperAdmin are always false from this entry point.
//
// When ParentRoleID is set, BOTH the role insert + the edge insert
// run inside one [pg.UnitOfWork] transaction so partial failure (e.g.
// the edge insert hits a cycle/cross-tenant DB rejection) rolls back
// the role insert too. ADR 0058.
func (h CreateRoleHandler) Handle(ctx context.Context, cmd CreateRoleCommand) (CreateRoleResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateRoleResult{}, errors.New("create_role: tenant id required")
	}
	now := h.now()
	r, err := role.New(role.ID(ids.NewV7().String()), cmd.TenantID, cmd.Name,
		false /* isSystemDefault */, cmd.HierarchyLevel, false /* isSuperAdmin */, now)
	if err != nil {
		return CreateRoleResult{}, err
	}

	// Plain-role-no-parent path: single Add, no UoW required.
	if cmd.ParentRoleID.IsZero() {
		if err := h.roles.Add(ctx, r); err != nil {
			if errors.Is(err, role.ErrNameTaken) {
				return CreateRoleResult{}, ErrRoleNameTaken
			}
			return CreateRoleResult{}, fmt.Errorf("create_role: %w", err)
		}
		return CreateRoleResult{RoleID: r.ID()}, nil
	}

	// Role + parent-edge path: atomic UoW. Cycle is impossible
	// (fresh role can't already be an ancestor), but cross-tenant +
	// parent-not-found still need the DB to reject — surfaces via
	// translateHierarchyEdgeError.
	if h.edges == nil || h.uow == nil {
		return CreateRoleResult{}, errors.New("create_role: parent edge requires edges repo + uow wiring")
	}
	edge, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		cmd.TenantID,
		r.ID(),
		cmd.ParentRoleID,
		cmd.ActorMembershipID,
		cmd.Reason,
		h.now(),
	)
	if err != nil {
		return CreateRoleResult{}, err
	}

	txErr := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := h.roles.Add(ctx, r); err != nil {
			return err
		}
		return h.edges.Add(ctx, edge)
	})
	if txErr != nil {
		if errors.Is(txErr, role.ErrNameTaken) {
			return CreateRoleResult{}, ErrRoleNameTaken
		}
		if errors.Is(txErr, rolehierarchy.ErrCycle) ||
			errors.Is(txErr, rolehierarchy.ErrCrossTenant) ||
			errors.Is(txErr, rolehierarchy.ErrSelfReference) ||
			errors.Is(txErr, rolehierarchy.ErrEdgeAlreadyExists) {
			return CreateRoleResult{}, txErr
		}
		return CreateRoleResult{}, fmt.Errorf("create_role: %w", txErr)
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
	now   func() time.Time
}

// NewUpdateRoleHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor. Nil → time.Now.
func NewUpdateRoleHandler(r role.Repository, now func() time.Time) UpdateRoleHandler {
	if r == nil {
		panic("command: NewUpdateRoleHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateRoleHandler{roles: r, now: now}
}

// Handle dispatches to the aggregate. System-default roles +
// SuperAdmin reject mutation per the aggregate's ensureMutable gate.
func (h UpdateRoleHandler) Handle(ctx context.Context, cmd UpdateRoleCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("update_role: role id required")
	}
	now := h.now()
	err := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		mutated := false
		if cmd.Name != "" {
			if err := r.Rename(cmd.Name, now); err != nil {
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
	now   func() time.Time
}

// NewReplaceRolePermissionsHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewReplaceRolePermissionsHandler(r role.Repository, now func() time.Time) ReplaceRolePermissionsHandler {
	if r == nil {
		panic("command: NewReplaceRolePermissionsHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ReplaceRolePermissionsHandler{roles: r, now: now}
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
	now := h.now()
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.ReplacePermissions(target, now); err != nil {
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
	now   func() time.Time
}

// NewGrantRolePermissionHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewGrantRolePermissionHandler(r role.Repository, now func() time.Time) GrantRolePermissionHandler {
	if r == nil {
		panic("command: NewGrantRolePermissionHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return GrantRolePermissionHandler{roles: r, now: now}
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
	now := h.now()
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.GrantPermission(p, now); err != nil {
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
	now   func() time.Time
}

// NewRevokeRolePermissionHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewRevokeRolePermissionHandler(r role.Repository, now func() time.Time) RevokeRolePermissionHandler {
	if r == nil {
		panic("command: NewRevokeRolePermissionHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RevokeRolePermissionHandler{roles: r, now: now}
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
	now := h.now()
	upErr := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.RevokePermission(p, now); err != nil {
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
	now   func() time.Time
}

// NewDeleteRoleHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor. Nil → time.Now.
func NewDeleteRoleHandler(r role.Repository, now func() time.Time) DeleteRoleHandler {
	if r == nil {
		panic("command: NewDeleteRoleHandler roles repository required")
	}
	if now == nil {
		now = time.Now
	}
	return DeleteRoleHandler{roles: r, now: now}
}

// Handle dispatches to [Role.Delete].
func (h DeleteRoleHandler) Handle(ctx context.Context, cmd DeleteRoleCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("delete_role: role id required")
	}
	now := h.now()
	err := h.roles.UpdateByID(ctx, cmd.RoleID, func(r *role.Role) (bool, error) {
		if err := r.Delete(cmd.DeletedBy, now); err != nil {
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

// ----- SetRoleParent (ADR 0058 — Wave 9.4) ----------------------------------

// SetRoleParentCommand carries the validated set-parent input.
//
// NewParentID == zero clears the parent (role becomes a root) by
// soft-deleting any existing active edge. When non-zero the handler
// atomically replaces any existing active edge: soft-delete old +
// insert new in one UoW transaction so the single-parent invariant
// holds without a window where the partial unique index would
// reject the insert.
//
// TenantID is the caller's tenant scope (injected from JWT context
// by the HTTP layer); composite FKs reject cross-tenant inserts.
//
// ActorMembershipID is the caller's membership — recorded on both
// the establish + remove sides for audit. Zero is tolerated (system /
// bootstrap paths) but HTTP callers always have one.
//
// Reason is optional; when supplied propagates onto the new edge
// (establishment) OR onto the cleared edge (removal). Subject to
// [rolehierarchy.MinReasonLength].
type SetRoleParentCommand struct {
	TenantID          tenant.ID
	RoleID            role.ID
	NewParentID       role.ID
	ActorMembershipID membership.ID
	Reason            string
}

// SetRoleParentHandler runs the set-parent flow.
type SetRoleParentHandler struct {
	edges rolehierarchy.Repository
	uow   pg.UnitOfWork
	now   func() time.Time
}

// NewSetRoleParentHandler wires the handler. uow may be nil only in
// the rare case where the caller is already inside a uow tx (test
// fixtures); production wiring always supplies one.
func NewSetRoleParentHandler(edges rolehierarchy.Repository, uow pg.UnitOfWork, now func() time.Time) SetRoleParentHandler {
	if edges == nil {
		panic("command: NewSetRoleParentHandler edges repository required")
	}
	if uow == nil {
		panic("command: NewSetRoleParentHandler uow required")
	}
	if now == nil {
		now = time.Now
	}
	return SetRoleParentHandler{edges: edges, uow: uow, now: now}
}

// Handle replaces the active parent edge for cmd.RoleID. Atomic via
// pg.UnitOfWork. Self-reference rejected at the aggregate; cycle +
// cross-tenant rejected at the DB layer (cycle trigger + composite
// FK fire on insert).
func (h SetRoleParentHandler) Handle(ctx context.Context, cmd SetRoleParentCommand) error {
	if cmd.RoleID.IsZero() {
		return errors.New("set_role_parent: role id required")
	}
	if cmd.NewParentID.IsZero() {
		// Clear-parent path: only soft-delete; no new edge to insert.
		return h.clearParent(ctx, cmd)
	}
	if cmd.TenantID.IsZero() {
		return errors.New("set_role_parent: tenant id required for non-clear path")
	}

	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		// Step 1: soft-delete any existing active edge for this child.
		// Idempotent — when the child has no parent, GetActiveByChild
		// returns ErrEdgeNotFound + we move on.
		if rmErr := h.removeExistingEdge(ctx, cmd); rmErr != nil {
			return rmErr
		}

		// Step 2: insert the new edge. Aggregate rejects self-reference;
		// DB rejects cycle + cross-tenant via composite FK / cycle trigger.
		edge, err := rolehierarchy.New(
			rolehierarchy.ID(ids.NewV7().String()),
			cmd.TenantID,
			cmd.RoleID,
			cmd.NewParentID,
			cmd.ActorMembershipID,
			cmd.Reason,
			h.now(),
		)
		if err != nil {
			return err
		}
		return h.edges.Add(ctx, edge)
	})
}

// clearParent handles the NewParentID-is-zero branch. Single
// soft-delete — no need to open a UoW since there's only one write.
func (h SetRoleParentHandler) clearParent(ctx context.Context, cmd SetRoleParentCommand) error {
	existing, gErr := h.edges.GetActiveByChild(ctx, cmd.RoleID)
	if errors.Is(gErr, rolehierarchy.ErrEdgeNotFound) {
		// Already a root — idempotent no-op.
		return nil
	}
	if gErr != nil {
		return fmt.Errorf("set_role_parent: load existing edge: %w", gErr)
	}
	now := h.now()
	rmErr := h.edges.UpdateByID(ctx, existing.ID(), func(e *rolehierarchy.Edge) (bool, error) {
		if err := e.Remove(cmd.ActorMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if rmErr != nil {
		return fmt.Errorf("set_role_parent: soft-delete existing: %w", rmErr)
	}
	return nil
}

// removeExistingEdge soft-deletes the current active edge (if any).
// Runs inside an open UoW tx (the soft-delete + the subsequent
// insert must commit atomically to preserve the single-parent
// invariant without a transient window where both rows are active).
func (h SetRoleParentHandler) removeExistingEdge(ctx context.Context, cmd SetRoleParentCommand) error {
	existing, gErr := h.edges.GetActiveByChild(ctx, cmd.RoleID)
	if errors.Is(gErr, rolehierarchy.ErrEdgeNotFound) {
		return nil
	}
	if gErr != nil {
		return fmt.Errorf("set_role_parent: load existing edge: %w", gErr)
	}
	now := h.now()
	return h.edges.UpdateByID(ctx, existing.ID(), func(e *rolehierarchy.Edge) (bool, error) {
		if err := e.Remove(cmd.ActorMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
}
