package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// User authorization mutations: role assignment + permission overlay
// + manager hierarchy. Each handler is a thin UpdateByID wrapper over
// the corresponding [membership.Membership] method.

// ----- AssignUserRole -------------------------------------------------------

// AssignUserRoleCommand grants a Role to a Membership. Aggregate
// dedups silently — re-assigning the same Role emits no event.
type AssignUserRoleCommand struct {
	MembershipID membership.ID
	RoleID       role.ID
}

// AssignUserRoleHandler runs the assignment.
type AssignUserRoleHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewAssignUserRoleHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewAssignUserRoleHandler(m membership.Repository, now func() time.Time) AssignUserRoleHandler {
	if m == nil {
		panic("command: NewAssignUserRoleHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return AssignUserRoleHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.AssignRole].
func (h AssignUserRoleHandler) Handle(ctx context.Context, cmd AssignUserRoleCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("assign_user_role: membership id required")
	}
	if cmd.RoleID.IsZero() {
		return errors.New("assign_user_role: role id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.AssignRole(cmd.RoleID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("assign_user_role: %w", err)
	}
	return nil
}

// ----- RevokeUserRole -------------------------------------------------------

// RevokeUserRoleCommand removes a Role assignment.
type RevokeUserRoleCommand struct {
	MembershipID membership.ID
	RoleID       role.ID
}

// RevokeUserRoleHandler runs the revocation.
type RevokeUserRoleHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewRevokeUserRoleHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRevokeUserRoleHandler(m membership.Repository, now func() time.Time) RevokeUserRoleHandler {
	if m == nil {
		panic("command: NewRevokeUserRoleHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RevokeUserRoleHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.RevokeRole].
func (h RevokeUserRoleHandler) Handle(ctx context.Context, cmd RevokeUserRoleCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("revoke_user_role: membership id required")
	}
	if cmd.RoleID.IsZero() {
		return errors.New("revoke_user_role: role id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.RevokeRole(cmd.RoleID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke_user_role: %w", err)
	}
	return nil
}

// ----- ReplaceUserPermissionOverrides --------------------------------------

// ReplaceUserPermissionOverridesCommand atomically replaces a
// Membership's grant + revoke overlays. Permission names MUST be
// catalogue-known per [permission.FromConstant]; unknown names panic
// at the boundary (programmer error, not request error — bad input
// from the HTTP layer is converted to ErrPermissionUnknown below).
type ReplaceUserPermissionOverridesCommand struct {
	MembershipID    membership.ID
	GrantedNames    []string
	RevokedNames    []string
}

// ErrPermissionUnknown surfaces when a permission name in the request
// doesn't match the closed [permission.IdentityPermissions] catalogue.
// Wire shape: 422 with the offending name in the message.
var ErrPermissionUnknown = errors.New("user: unknown permission")

// ReplaceUserPermissionOverridesHandler runs the overlay replacement.
type ReplaceUserPermissionOverridesHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewReplaceUserPermissionOverridesHandler wires the handler. `now` is
// the explicit time source per the clock-injection refactor. Nil → time.Now.
func NewReplaceUserPermissionOverridesHandler(m membership.Repository, now func() time.Time) ReplaceUserPermissionOverridesHandler {
	if m == nil {
		panic("command: NewReplaceUserPermissionOverridesHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ReplaceUserPermissionOverridesHandler{memberships: m, now: now}
}

// Handle resolves each permission name through the closed catalogue
// + dispatches to [Membership.ReplacePermissionOverlays].
func (h ReplaceUserPermissionOverridesHandler) Handle(ctx context.Context, cmd ReplaceUserPermissionOverridesCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("replace_user_permission_overrides: membership id required")
	}
	granted, err := resolvePermissions(cmd.GrantedNames)
	if err != nil {
		return err
	}
	revoked, err := resolvePermissions(cmd.RevokedNames)
	if err != nil {
		return err
	}
	now := h.now()
	upErr := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.ReplacePermissionOverlays(granted, revoked, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(upErr, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if upErr != nil {
		return fmt.Errorf("replace_user_permission_overrides: %w", upErr)
	}
	return nil
}

// resolvePermissions maps untrusted permission-name strings to
// interned [permission.Permission] entries. Returns
// [ErrPermissionUnknown] (wrapped) on the first unknown name so the
// HTTP layer can surface a 422 with the specific offender.
func resolvePermissions(names []string) ([]*permission.Permission, error) {
	out := make([]*permission.Permission, 0, len(names))
	for _, n := range names {
		p, err := permission.TryFromConstant(n)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrPermissionUnknown, n)
		}
		out = append(out, p)
	}
	return out, nil
}

// ----- AssignUserManager ---------------------------------------------------

// AssignUserManagerCommand sets the supplied Membership's reports-to
// pointer. Same-tenant invariant enforced at DB level by the composite
// FK (membership_id, tenant_id) → tenant_memberships(id, tenant_id).
type AssignUserManagerCommand struct {
	MembershipID membership.ID
	ManagerID    membership.ID
}

// AssignUserManagerHandler runs the manager assignment.
type AssignUserManagerHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewAssignUserManagerHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewAssignUserManagerHandler(m membership.Repository, now func() time.Time) AssignUserManagerHandler {
	if m == nil {
		panic("command: NewAssignUserManagerHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return AssignUserManagerHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.AssignManager]. Self-management
// (m.id == cmd.ManagerID) is rejected by the aggregate.
func (h AssignUserManagerHandler) Handle(ctx context.Context, cmd AssignUserManagerCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("assign_user_manager: membership id required")
	}
	if cmd.ManagerID.IsZero() {
		return errors.New("assign_user_manager: manager id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.AssignManager(cmd.ManagerID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("assign_user_manager: %w", err)
	}
	return nil
}

// ----- RemoveUserManager ---------------------------------------------------

// RemoveUserManagerCommand clears the reports-to pointer.
type RemoveUserManagerCommand struct {
	MembershipID membership.ID
}

// RemoveUserManagerHandler runs the manager removal.
type RemoveUserManagerHandler struct {
	memberships membership.Repository
	now         func() time.Time
}

// NewRemoveUserManagerHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRemoveUserManagerHandler(m membership.Repository, now func() time.Time) RemoveUserManagerHandler {
	if m == nil {
		panic("command: NewRemoveUserManagerHandler memberships repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RemoveUserManagerHandler{memberships: m, now: now}
}

// Handle dispatches to [Membership.RemoveManager]. Idempotent —
// already-no-manager returns nil without an event.
func (h RemoveUserManagerHandler) Handle(ctx context.Context, cmd RemoveUserManagerCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("remove_user_manager: membership id required")
	}
	now := h.now()
	err := h.memberships.UpdateByID(ctx, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
		if err := m.RemoveManager(now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, membership.ErrNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("remove_user_manager: %w", err)
	}
	return nil
}
