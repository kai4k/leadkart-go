package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
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

// Role commands decide through the authority policy (ADR 0069) inside the
// target tenant's authority lock and ONE transaction: a role's permissions
// reach every holder, so role writes serialise with membership writes. The
// Actor is built once by the port (authn.AuthorityFrom); the zero Actor is
// refused before anything is locked or loaded.

// ErrRoleInUse surfaces when a delete is refused because active memberships
// still hold the role. Wire shape: 409 role_in_use with the holder count
// (see [RoleInUseError]).
var ErrRoleInUse = errors.New("role: in use by active members")

// RoleInUseError is [ErrRoleInUse] with the number of active holders.
type RoleInUseError struct{ ActiveHolders int }

func (e *RoleInUseError) Error() string {
	return fmt.Sprintf("%s: %d active holders", ErrRoleInUse, e.ActiveHolders)
}

// Is makes errors.Is(err, ErrRoleInUse) match.
func (e *RoleInUseError) Is(target error) bool { return target == ErrRoleInUse }

// roleWriter is the shared base of the role handlers: the tenant lock, the
// facts loader and one clock.
type roleWriter struct{ authorityWriter }

// updateRole runs fn (decide, then mutate) on the live role under the lock;
// a soft-deleted or foreign role is role.ErrNotFound.
func (w roleWriter) updateRole(ctx context.Context, tenantID tenant.ID, id role.ID, actor authority.Actor, now time.Time, fn func(actor authority.Actor, r *role.Role) error) error {
	return w.locked(ctx, tenantID, actor, now, func(ctx context.Context, actor authority.Actor) error {
		return w.facts.roles.UpdateByID(ctx, tenantID, id, func(r *role.Role) (bool, error) {
			if err := fn(actor, r); err != nil {
				return false, err
			}
			return true, nil
		})
	})
}

func validateRoleCommand(op string, tenantID tenant.ID, id role.ID, needID bool, actor authority.Actor) error {
	if tenantID.IsZero() {
		return errors.New(op + ": tenant id required")
	}
	if needID && id.IsZero() {
		return errors.New(op + ": role id required")
	}
	if !actor.Valid() {
		return authority.ErrNoAuthority
	}
	return nil
}

// ----- CreateRole ----------------------------------------------------------

// CreateRoleCommand carries the validated create-role input. TenantID
// arrives from the caller's JWT — caller doesn't pick a tenant.
//
// IsSuperAdmin is intentionally NOT exposed: the SuperAdmin role is
// seed-only per multi-tenancy.md "SuperUser god-mode". Tenant admins
// cannot promote a custom role to SuperAdmin via HTTP.
//
// ParentRoleID (ADR 0058) is an optional parent; zero = root. The role and
// its edge are written in the same transaction.
//
// ActorMembershipID is recorded on the edge for audit; zero is tolerated
// (operator header scope, system paths).
type CreateRoleCommand struct {
	TenantID          tenant.ID
	Name              string
	HierarchyLevel    int
	ParentRoleID      role.ID
	ActorMembershipID membership.ID
	Reason            string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// CreateRoleResult returns the new role's ID for the caller.
type CreateRoleResult struct {
	RoleID role.ID
}

// CreateRoleHandler runs the create flow.
type CreateRoleHandler struct {
	roleWriter
	edges     rolehierarchy.Repository
	newRoleID func() role.ID
	newEdgeID func() rolehierarchy.ID
}

// NewCreateRoleHandler wires the handler. newRoleID + newEdgeID are the Pure
// Domain ID factories per ADR 0047.
func NewCreateRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository,
	edges rolehierarchy.Repository, now func() time.Time, newRoleID func() role.ID, newEdgeID func() rolehierarchy.ID) CreateRoleHandler {
	if edges == nil {
		panic("command: NewCreateRoleHandler edges repository required")
	}
	if newRoleID == nil {
		panic("command: NewCreateRoleHandler newRoleID required")
	}
	if newEdgeID == nil {
		panic("command: NewCreateRoleHandler newEdgeID required")
	}
	return CreateRoleHandler{
		roleWriter: roleWriter{newAuthorityWriter("NewCreateRoleHandler", uow, lock, m, r, p, now)},
		edges:      edges, newRoleID: newRoleID, newEdgeID: newEdgeID,
	}
}

// Handle decides [authority.Actor.MayCreateRole] (parent loaded when given),
// then writes the role and its parent edge in the one locked transaction.
func (h CreateRoleHandler) Handle(ctx context.Context, cmd CreateRoleCommand) (CreateRoleResult, error) {
	if err := validateRoleCommand("create_role", cmd.TenantID, "", false, cmd.Actor); err != nil {
		return CreateRoleResult{}, err
	}
	now := h.now()
	var created role.ID
	err := h.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
		var parent *role.Role
		if !cmd.ParentRoleID.IsZero() {
			p, err := h.facts.role(ctx, cmd.TenantID, cmd.ParentRoleID)
			if errors.Is(err, role.ErrNotFound) {
				return fmt.Errorf("%w: parent role not found in this tenant", authority.ErrRoleParentNotEligible)
			}
			if err != nil {
				return err
			}
			parent = p
		}
		if err := actor.MayCreateRole(cmd.TenantID, cmd.HierarchyLevel, nil, parent); err != nil {
			return err
		}
		r, err := role.New(h.newRoleID(), cmd.TenantID, cmd.Name,
			false /* isSystemDefault */, cmd.HierarchyLevel, false /* isSuperAdmin */, now)
		if err != nil {
			return err
		}
		if err := h.facts.roles.Add(ctx, r); err != nil {
			return err
		}
		if parent != nil {
			edge, err := rolehierarchy.New(h.newEdgeID(), cmd.TenantID, r.ID(), parent.ID(), cmd.ActorMembershipID, cmd.Reason, now)
			if err != nil {
				return err
			}
			if err := h.edges.Add(ctx, edge); err != nil {
				return err
			}
		}
		created = r.ID()
		return nil
	})
	if err != nil {
		return CreateRoleResult{}, translateAuthorityWrite("create_role", err)
	}
	return CreateRoleResult{RoleID: created}, nil
}

// ----- UpdateRole ----------------------------------------------------------

// UpdateRoleCommand renames and/or re-levels a role. Empty Name = no rename;
// HierarchyLevel < 0 (-1) = no change.
type UpdateRoleCommand struct {
	TenantID       tenant.ID
	RoleID         role.ID
	Name           string // empty = no rename
	HierarchyLevel int    // -1 = no change
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// UpdateRoleHandler runs the update flow.
type UpdateRoleHandler struct{ roleWriter }

// NewUpdateRoleHandler wires the handler.
func NewUpdateRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) UpdateRoleHandler {
	return UpdateRoleHandler{roleWriter{newAuthorityWriter("NewUpdateRoleHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayEditRole], then renames / re-levels.
func (h UpdateRoleHandler) Handle(ctx context.Context, cmd UpdateRoleCommand) error {
	if err := validateRoleCommand("update_role", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	now := h.now()
	newLevel := authority.NoLevelChange
	if cmd.HierarchyLevel >= 0 {
		newLevel = cmd.HierarchyLevel
	}
	err := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, func(actor authority.Actor, r *role.Role) error {
		if err := actor.MayEditRole(r, newLevel); err != nil {
			return err
		}
		if cmd.Name != "" {
			if err := r.Rename(cmd.Name, now); err != nil {
				return err
			}
		}
		if newLevel != authority.NoLevelChange {
			return r.ChangeHierarchyLevel(newLevel)
		}
		return nil
	})
	return translateAuthorityWrite("update_role", err)
}

// ----- ReplaceRolePermissions ----------------------------------------------

// ReplaceRolePermissionsCommand atomically replaces the role's permission
// set. Names MUST be catalogue-known (ErrPermissionUnknown otherwise). An
// empty (non-nil) set clears the role; the port refuses a missing key.
type ReplaceRolePermissionsCommand struct {
	TenantID        tenant.ID
	RoleID          role.ID
	PermissionNames []string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// ReplaceRolePermissionsHandler runs the replace flow.
type ReplaceRolePermissionsHandler struct{ roleWriter }

// NewReplaceRolePermissionsHandler wires the handler.
func NewReplaceRolePermissionsHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ReplaceRolePermissionsHandler {
	return ReplaceRolePermissionsHandler{roleWriter{newAuthorityWriter("NewReplaceRolePermissionsHandler", uow, lock, m, r, p, now)}}
}

// Handle resolves names, decides [authority.Actor.MayReplaceRolePermissions]
// on the delta against the stored set, then replaces.
func (h ReplaceRolePermissionsHandler) Handle(ctx context.Context, cmd ReplaceRolePermissionsCommand) error {
	if err := validateRoleCommand("replace_role_permissions", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	target, err := resolvePermissions(cmd.PermissionNames)
	if err != nil {
		return err
	}
	now := h.now()
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, func(actor authority.Actor, r *role.Role) error {
		if err := actor.MayReplaceRolePermissions(r, authority.PermissionNames(target)); err != nil {
			return err
		}
		return r.ReplacePermissions(target, now)
	})
	return translateAuthorityWrite("replace_role_permissions", upErr)
}

// ----- GrantRolePermission --------------------------------------------------

// GrantRolePermissionCommand adds a single permission to the role.
type GrantRolePermissionCommand struct {
	TenantID       tenant.ID
	RoleID         role.ID
	PermissionName string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// GrantRolePermissionHandler runs the grant flow.
type GrantRolePermissionHandler struct{ roleWriter }

// NewGrantRolePermissionHandler wires the handler.
func NewGrantRolePermissionHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) GrantRolePermissionHandler {
	return GrantRolePermissionHandler{roleWriter{newAuthorityWriter("NewGrantRolePermissionHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayGrantRolePermission], then grants.
func (h GrantRolePermissionHandler) Handle(ctx context.Context, cmd GrantRolePermissionCommand) error {
	if err := validateRoleCommand("grant_role_permission", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	p, err := permission.TryFromConstant(cmd.PermissionName)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrPermissionUnknown, cmd.PermissionName)
	}
	now := h.now()
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, func(actor authority.Actor, r *role.Role) error {
		if err := actor.MayGrantRolePermission(r, p.Name()); err != nil {
			return err
		}
		return r.GrantPermission(p, now)
	})
	return translateAuthorityWrite("grant_role_permission", upErr)
}

// ----- RevokeRolePermission -------------------------------------------------

// RevokeRolePermissionCommand removes a single permission.
type RevokeRolePermissionCommand struct {
	TenantID       tenant.ID
	RoleID         role.ID
	PermissionName string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// RevokeRolePermissionHandler runs the revoke flow.
type RevokeRolePermissionHandler struct{ roleWriter }

// NewRevokeRolePermissionHandler wires the handler.
func NewRevokeRolePermissionHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) RevokeRolePermissionHandler {
	return RevokeRolePermissionHandler{roleWriter{newAuthorityWriter("NewRevokeRolePermissionHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayRevokeRolePermission], then revokes.
func (h RevokeRolePermissionHandler) Handle(ctx context.Context, cmd RevokeRolePermissionCommand) error {
	if err := validateRoleCommand("revoke_role_permission", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	p, err := permission.TryFromConstant(cmd.PermissionName)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrPermissionUnknown, cmd.PermissionName)
	}
	now := h.now()
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, func(actor authority.Actor, r *role.Role) error {
		if err := actor.MayRevokeRolePermission(r, p.Name()); err != nil {
			return err
		}
		return r.RevokePermission(p, now)
	})
	return translateAuthorityWrite("revoke_role_permission", upErr)
}

// ----- DeleteRole ----------------------------------------------------------

// DeleteRoleCommand soft-deletes a role. DeletedBy is the caller's PersonID
// (from the JWT Subject claim) — recorded for audit.
type DeleteRoleCommand struct {
	TenantID  tenant.ID
	RoleID    role.ID
	DeletedBy string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// DeleteRoleHandler runs the delete flow.
type DeleteRoleHandler struct{ roleWriter }

// NewDeleteRoleHandler wires the handler.
func NewDeleteRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) DeleteRoleHandler {
	return DeleteRoleHandler{roleWriter{newAuthorityWriter("NewDeleteRoleHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayDeleteRole] (authority first), then
// applies the role-in-use rule: an ACTIVE holder refuses the delete
// ([RoleInUseError], nothing changes); otherwise the role is deleted and the
// remaining (inactive members') assignments are revoked in the same
// transaction, so no membership is left pointing at a deleted role. The
// authority-facts loader tolerates soft-deleted roles on assigned members
// only as a backstop for rows that predate this rule.
func (h DeleteRoleHandler) Handle(ctx context.Context, cmd DeleteRoleCommand) error {
	if err := validateRoleCommand("delete_role", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	now := h.now()
	err := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, func(actor authority.Actor, r *role.Role) error {
		if err := actor.MayDeleteRole(r); err != nil {
			return err
		}
		holders, err := h.facts.memberships.ListHoldingRole(ctx, cmd.TenantID, cmd.RoleID)
		if err != nil {
			return fmt.Errorf("list holders: %w", err)
		}
		active := 0
		for _, m := range holders {
			if m.Status() == membership.StatusActive {
				active++
			}
		}
		if active > 0 {
			return &RoleInUseError{ActiveHolders: active}
		}
		for _, m := range holders {
			if err := h.facts.memberships.UpdateByID(ctx, cmd.TenantID, m.ID(), func(held *membership.Membership) (bool, error) {
				return true, held.RevokeRole(cmd.RoleID, now)
			}); err != nil {
				return fmt.Errorf("revoke deleted role from %s: %w", m.ID(), err)
			}
		}
		return r.Delete(cmd.DeletedBy, now)
	})
	return translateAuthorityWrite("delete_role", err)
}

// ----- SetRoleParent (ADR 0058 — Wave 9.4) ----------------------------------

// SetRoleParentCommand carries the validated set-parent input.
//
// NewParentID == zero clears the parent (role becomes a root) by
// soft-deleting any existing active edge. When non-zero the handler
// atomically replaces any existing active edge in one transaction so the
// single-parent invariant holds. ActorMembershipID is recorded on the edge
// for audit (zero tolerated). Reason is optional ([rolehierarchy.MinReasonLength]).
type SetRoleParentCommand struct {
	TenantID          tenant.ID
	RoleID            role.ID
	NewParentID       role.ID
	ActorMembershipID membership.ID
	Reason            string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// SetRoleParentHandler runs the set-parent flow.
type SetRoleParentHandler struct {
	roleWriter
	edges     rolehierarchy.Repository
	newEdgeID func() rolehierarchy.ID
}

// NewSetRoleParentHandler wires the handler. newEdgeID is the Pure Domain ID
// factory (ADR 0047).
func NewSetRoleParentHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository,
	edges rolehierarchy.Repository, now func() time.Time, newEdgeID func() rolehierarchy.ID) SetRoleParentHandler {
	if edges == nil {
		panic("command: NewSetRoleParentHandler edges repository required")
	}
	if newEdgeID == nil {
		panic("command: NewSetRoleParentHandler newEdgeID required")
	}
	return SetRoleParentHandler{
		roleWriter: roleWriter{newAuthorityWriter("NewSetRoleParentHandler", uow, lock, m, r, p, now)},
		edges:      edges, newEdgeID: newEdgeID,
	}
}

// Handle decides [authority.Actor.MaySetRoleParent] (child and parent loaded
// under the lock; a missing parent is not eligible), then replaces the active
// parent edge. Self-reference is rejected at the aggregate; a cycle by the DB
// trigger.
func (h SetRoleParentHandler) Handle(ctx context.Context, cmd SetRoleParentCommand) error {
	if err := validateRoleCommand("set_role_parent", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	now := h.now()
	err := h.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
		child, err := h.facts.role(ctx, cmd.TenantID, cmd.RoleID)
		if err != nil {
			return err
		}
		if child.IsDeleted() {
			return role.ErrNotFound
		}
		var parent *role.Role
		if !cmd.NewParentID.IsZero() {
			parent, err = h.facts.role(ctx, cmd.TenantID, cmd.NewParentID)
			if errors.Is(err, role.ErrNotFound) {
				return fmt.Errorf("%w: parent role not found in this tenant", authority.ErrRoleParentNotEligible)
			}
			if err != nil {
				return err
			}
		}
		if err := actor.MaySetRoleParent(child, parent); err != nil {
			return err
		}
		existing, gErr := h.edges.GetActiveByChild(ctx, cmd.TenantID, cmd.RoleID)
		switch {
		case errors.Is(gErr, rolehierarchy.ErrEdgeNotFound):
			existing = nil
		case gErr != nil:
			return fmt.Errorf("load existing edge: %w", gErr)
		}
		if existing != nil {
			if err := h.edges.UpdateByID(ctx, cmd.TenantID, existing.ID(), func(e *rolehierarchy.Edge) (bool, error) {
				return true, e.Remove(cmd.ActorMembershipID, cmd.Reason, now)
			}); err != nil {
				return fmt.Errorf("soft-delete existing edge: %w", err)
			}
		}
		if parent == nil {
			return nil
		}
		edge, err := rolehierarchy.New(h.newEdgeID(), cmd.TenantID, cmd.RoleID, cmd.NewParentID, cmd.ActorMembershipID, cmd.Reason, now)
		if err != nil {
			return err
		}
		return h.edges.Add(ctx, edge)
	})
	return translateAuthorityWrite("set_role_parent", err)
}
