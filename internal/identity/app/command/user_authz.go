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
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// User authorization mutations: role assignment + permission overlay
// + manager hierarchy. Each handler decides through the authority policy
// (ADR 0069) inside ONE transaction that holds the target tenant's
// authority lock, on facts loaded in that transaction; the mutation is the
// corresponding [membership.Membership] method.
//
// Per ADR 0062 every command here carries the TenantID explicitly (the
// HTTP layer projects it from the request's effective tenant) and the
// Actor, built once by the port from verified claims. A zero Actor is
// refused before anything is locked or loaded.

// ----- AssignUserRole -------------------------------------------------------

// AssignUserRoleCommand grants a Role to a Membership. Aggregate
// dedups silently — re-assigning the same Role emits no event.
type AssignUserRoleCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	RoleID       role.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// AssignUserRoleHandler runs the assignment.
type AssignUserRoleHandler struct{ w authorityWriter }

// NewAssignUserRoleHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewAssignUserRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) AssignUserRoleHandler {
	return AssignUserRoleHandler{w: newAuthorityWriter("NewAssignUserRoleHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayAssignRole] under the tenant lock, then
// dispatches to [Membership.AssignRole].
func (h AssignUserRoleHandler) Handle(ctx context.Context, cmd AssignUserRoleCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("assign_user_role: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("assign_user_role: membership id required")
	}
	if cmd.RoleID.IsZero() {
		return errors.New("assign_user_role: role id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		r, err := h.w.facts.role(ctx, cmd.TenantID, cmd.RoleID)
		if err != nil {
			return err
		}
		if r.IsDeleted() {
			return role.ErrNotFound // a deleted role is not assignable: same answer as a missing one
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
			target, err := h.w.facts.member(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			if err := cmd.Actor.MayAssignRole(target, r); err != nil {
				return false, err
			}
			return true, m.AssignRole(cmd.RoleID, now)
		})
	})
	return translateAuthorityWrite("assign_user_role", err)
}

// ----- RevokeUserRole -------------------------------------------------------

// RevokeUserRoleCommand removes a Role assignment.
type RevokeUserRoleCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	RoleID       role.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// RevokeUserRoleHandler runs the revocation.
type RevokeUserRoleHandler struct{ w authorityWriter }

// NewRevokeUserRoleHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRevokeUserRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) RevokeUserRoleHandler {
	return RevokeUserRoleHandler{w: newAuthorityWriter("NewRevokeUserRoleHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayRevokeRole] under the tenant lock (the
// owner set is loaded when the role is Company Owner, the only case the
// policy reads it), then dispatches to [Membership.RevokeRole].
func (h RevokeUserRoleHandler) Handle(ctx context.Context, cmd RevokeUserRoleCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("revoke_user_role: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("revoke_user_role: membership id required")
	}
	if cmd.RoleID.IsZero() {
		return errors.New("revoke_user_role: role id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		r, err := h.w.facts.role(ctx, cmd.TenantID, cmd.RoleID)
		if errors.Is(err, role.ErrNotFound) {
			r = nil // not a role of this tenant: nothing is conferred, only rank applies
		} else if err != nil {
			return err
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, cmd.MembershipID, func(m *membership.Membership) (bool, error) {
			target, err := h.w.facts.member(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			if r == nil {
				err = cmd.Actor.MayActOn(target)
			} else {
				var owners []authority.Member
				if r.IsCompanyOwner() {
					if owners, err = h.w.facts.owners(ctx, cmd.TenantID, now); err != nil {
						return false, err
					}
				}
				err = cmd.Actor.MayRevokeRole(target, r, owners)
			}
			if err != nil {
				return false, err
			}
			return true, m.RevokeRole(cmd.RoleID, now)
		})
	})
	return translateAuthorityWrite("revoke_user_role", err)
}

// ----- ReplaceUserPermissionOverrides --------------------------------------

// ReplaceUserPermissionOverridesCommand atomically replaces a
// Membership's grant + revoke overlays. Permission names MUST be
// catalogue-known per [permission.FromConstant]; unknown names panic
// at the boundary (programmer error, not request error — bad input
// from the HTTP layer is converted to ErrPermissionUnknown below).
type ReplaceUserPermissionOverridesCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	GrantedNames []string
	RevokedNames []string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// ErrPermissionUnknown surfaces when a permission name in the request
// doesn't match the closed [permission.IdentityPermissions] catalogue.
// Wire shape: 422 with the offending name in the message.
var ErrPermissionUnknown = errors.New("user: unknown permission")

// ReplaceUserPermissionOverridesHandler runs the overlay replacement.
type ReplaceUserPermissionOverridesHandler struct{ w authorityWriter }

// NewReplaceUserPermissionOverridesHandler wires the handler. `now` is
// the explicit time source per the clock-injection refactor. Nil → time.Now.
func NewReplaceUserPermissionOverridesHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ReplaceUserPermissionOverridesHandler {
	return ReplaceUserPermissionOverridesHandler{w: newAuthorityWriter("NewReplaceUserPermissionOverridesHandler", uow, lock, m, r, p, now)}
}

// Handle resolves each permission name through the closed catalogue,
// decides [authority.Actor.MayChangeOverrides] under the tenant lock and
// dispatches to [Membership.ReplacePermissionOverrides].
func (h ReplaceUserPermissionOverridesHandler) Handle(ctx context.Context, cmd ReplaceUserPermissionOverridesCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("replace_user_permission_overrides: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("replace_user_permission_overrides: membership id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	granted, err := resolvePermissions(cmd.GrantedNames)
	if err != nil {
		return err
	}
	revoked, err := resolvePermissions(cmd.RevokedNames)
	if err != nil {
		return err
	}
	now := h.w.now()
	upErr := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, now, func(_ context.Context, m *membership.Membership, target authority.Member) error {
		after, approved := overridesAfter(target, granted, revoked)
		if err := cmd.Actor.MayChangeOverrides(target, after); err != nil {
			return err
		}
		// The aggregate writes exactly what the policy approved, at the one
		// clock reading taken above.
		return m.ReplacePermissionOverrides(approved, revoked, now)
	})
	return translateAuthorityWrite("replace_user_permission_overrides", upErr)
}

// overridesAfter is the override state a replace would leave, as the policy
// input and as the aggregate input (the same entries): a name that is already
// a live grant (at the snapshot's instant) and stays granted keeps its
// existing expiry; every other granted name is permanent.
func overridesAfter(target authority.Member, granted, revoked []*permission.Permission) (authority.Overrides, []membership.GrantedOverride) {
	live := make(map[string]time.Time)
	for _, g := range target.Overrides().Granted {
		live[g.Name] = g.ExpiresAt
	}
	after := authority.Overrides{Granted: make([]authority.Override, 0, len(granted)), Revoked: authority.PermissionNames(revoked)}
	approved := make([]membership.GrantedOverride, 0, len(granted))
	for _, p := range granted {
		if p == nil {
			continue
		}
		exp := live[p.Name()]
		after.Granted = append(after.Granted, authority.Override{Name: p.Name(), ExpiresAt: exp})
		approved = append(approved, membership.GrantedOverride{Permission: p, ExpiresAt: exp})
	}
	return after, approved
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
	TenantID     tenant.ID
	MembershipID membership.ID
	ManagerID    membership.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// AssignUserManagerHandler runs the manager assignment.
type AssignUserManagerHandler struct{ w authorityWriter }

// NewAssignUserManagerHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewAssignUserManagerHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) AssignUserManagerHandler {
	return AssignUserManagerHandler{w: newAuthorityWriter("NewAssignUserManagerHandler", uow, lock, m, r, p, now)}
}

// Handle walks the proposed manager's chain for a cycle, decides
// [authority.Actor.MayReplaceManager] under the tenant lock and dispatches
// to [Membership.AssignManager]. Self-management is rejected up front.
func (h AssignUserManagerHandler) Handle(ctx context.Context, cmd AssignUserManagerCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("assign_user_manager: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("assign_user_manager: membership id required")
	}
	if cmd.ManagerID.IsZero() {
		return errors.New("assign_user_manager: manager id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	mgrID, err := membership.ParseID(string(cmd.ManagerID))
	if err != nil {
		return err
	}
	subID, err := membership.ParseID(string(cmd.MembershipID))
	if err != nil {
		return err
	}
	if mgrID == subID {
		return fmt.Errorf("%w: cannot report to self", membership.ErrInvalid)
	}
	now := h.w.now()
	err = h.w.locked(ctx, cmd.TenantID, func(ctx context.Context) error {
		mgrM, mgr, err := h.w.facts.memberByID(ctx, cmd.TenantID, mgrID, now)
		if errors.Is(err, membership.ErrNotFound) {
			return fmt.Errorf("%w: manager not found in tenant", authority.ErrManagerNotEligible)
		}
		if err != nil {
			return err
		}
		if err := h.w.facts.rejectManagerCycle(ctx, cmd.TenantID, mgrM, subID); err != nil {
			return err
		}
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, subID, func(m *membership.Membership) (bool, error) {
			sub, err := h.w.facts.member(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			cur, found, err := h.w.facts.currentManager(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			if err := cmd.Actor.MayReplaceManager(sub, mgr, asCurrent(cur, found)); err != nil {
				return false, err
			}
			return true, m.AssignManager(mgrID, now)
		})
	})
	return translateAuthorityWrite("assign_user_manager", err)
}

// ----- RemoveUserManager ---------------------------------------------------

// RemoveUserManagerCommand clears the reports-to pointer.
type RemoveUserManagerCommand struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// RemoveUserManagerHandler runs the manager removal.
type RemoveUserManagerHandler struct{ w authorityWriter }

// NewRemoveUserManagerHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRemoveUserManagerHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) RemoveUserManagerHandler {
	return RemoveUserManagerHandler{w: newAuthorityWriter("NewRemoveUserManagerHandler", uow, lock, m, r, p, now)}
}

// Handle decides [authority.Actor.MayClearManager] under the tenant lock and
// dispatches to [Membership.RemoveManager]. Idempotent — already-no-manager
// returns nil without an event (the call is still authorised).
func (h RemoveUserManagerHandler) Handle(ctx context.Context, cmd RemoveUserManagerCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("remove_user_manager: tenant id required")
	}
	if cmd.MembershipID.IsZero() {
		return errors.New("remove_user_manager: membership id required")
	}
	if !cmd.Actor.Valid() {
		return authority.ErrNoAuthority
	}
	now := h.w.now()
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, now, func(ctx context.Context, m *membership.Membership, target authority.Member) error {
		cur, found, err := h.w.facts.currentManager(ctx, cmd.TenantID, m, now)
		if err != nil {
			return err
		}
		if err := cmd.Actor.MayClearManager(target, asCurrent(cur, found)); err != nil {
			return err
		}
		return m.RemoveManager(now)
	})
	return translateAuthorityWrite("remove_user_manager", err)
}
