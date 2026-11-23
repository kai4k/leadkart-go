package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	err := h.w.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
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
			if err := actor.MayAssignRole(target, r); err != nil {
				return false, err
			}
			if err := m.AssignRole(cmd.RoleID, now); err != nil {
				return false, err
			}
			return true, h.w.rotateStampIfChanged(ctx, m, "role_assigned", now)
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
	err := h.w.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
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
				err = actor.MayActOn(target)
			} else {
				var owners []authority.Member
				if r.IsCompanyOwner() {
					if owners, err = h.w.facts.owners(ctx, cmd.TenantID, now); err != nil {
						return false, err
					}
				}
				err = actor.MayRevokeRole(target, r, owners)
			}
			if err != nil {
				return false, err
			}
			if err := m.RevokeRole(cmd.RoleID, now); err != nil {
				return false, err
			}
			return true, h.w.rotateStampIfChanged(ctx, m, "role_revoked", now)
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
	// OverridesVersion is the [authority.Overrides.Version] of the live overrides
	// the caller's form was opened on (the single-user read hands it out). The
	// save is applied only while the live overrides still have that version.
	OverridesVersion string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// ErrOverridesChanged surfaces when the member's live overrides are no longer
// the ones the save was made from (its OverridesVersion does not match): a
// time-bound grant expired, a request was approved, or someone else saved.
// Nothing is written. Wire shape: 409 overrides_changed; the client reads the
// member again and the admin repeats the change on what is there now.
var ErrOverridesChanged = errors.New("user: the permission overrides changed since they were read")

// ErrPermissionUnknown surfaces when a permission name in the request
// doesn't match the closed [permission.IdentityPermissions] catalogue.
// Wire shape: 422 with the offending name in the message.
var ErrPermissionUnknown = errors.New("user: unknown permission")

// ErrOverridesConflict surfaces when a permission name is in both the granted
// and the revoked list of one request. Wire shape: 422 validation_failed with
// the offending names under errors.revoked.
var ErrOverridesConflict = errors.New("user: a permission cannot be both granted and revoked")

// OverridesConflictError carries the names listed in both lists.
type OverridesConflictError struct{ Names []string }

func (e *OverridesConflictError) Error() string {
	return fmt.Sprintf("%s: %s", ErrOverridesConflict, strings.Join(e.Names, ", "))
}

// Is makes errors.Is(err, ErrOverridesConflict) match.
func (e *OverridesConflictError) Is(target error) bool { return target == ErrOverridesConflict }

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
	// A name listed twice is one name: without this the after-state would hold the
	// name twice, and a change that drops OTHER grants could be mistaken for no change.
	granted = dedupeByName(granted)
	revoked = dedupeByName(revoked)
	if both := namesInBoth(granted, revoked); len(both) > 0 {
		return &OverridesConflictError{Names: both}
	}
	now := h.w.now()
	upErr := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error {
		// Authority first: a caller who may not act on this member is refused by
		// rank whatever version they sent, so the answer never says whether a
		// guessed version matched.
		if err := actor.MayActOn(target); err != nil {
			return err
		}
		// The precondition, on the live overrides read under the tenant authority
		// lock at this command's one clock reading: a save made from any other
		// state (a grant has expired since, an approval landed, someone else saved)
		// is refused whole. Without it a re-sent name whose time-bound grant had
		// expired would be stored PERMANENT, and a grant approved after the form
		// opened would be erased. The version is never empty, so no version matches
		// nothing.
		if target.Overrides().Version() != cmd.OverridesVersion {
			return ErrOverridesChanged
		}
		after, approved := overridesAfter(target, granted, revoked)
		if err := actor.MayChangeOverrides(target, after); err != nil {
			return err
		}
		if after.Equal(target.Overrides()) {
			// The same live overrides as stored: write nothing, record no event and
			// rotate nobody's stamp (a replay could otherwise force silent refreshes).
			return errNothingToWrite
		}
		// The aggregate writes exactly what the policy approved, at the one
		// clock reading taken above.
		if err := m.ReplacePermissionOverrides(approved, revoked, now); err != nil {
			return err
		}
		return h.w.rotateStampIfChanged(ctx, m, "overrides_replaced", now)
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

// dedupeByName keeps the first entry of each permission name.
func dedupeByName(in []*permission.Permission) []*permission.Permission {
	seen := make(map[string]struct{}, len(in))
	out := make([]*permission.Permission, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p.Name()]; dup {
			continue
		}
		seen[p.Name()] = struct{}{}
		out = append(out, p)
	}
	return out
}

// namesInBoth lists, sorted, the names present in both slices.
func namesInBoth(a, b []*permission.Permission) []string {
	inA := make(map[string]struct{}, len(a))
	for _, p := range a {
		inA[p.Name()] = struct{}{}
	}
	var out []string
	for _, p := range b {
		if _, ok := inA[p.Name()]; ok {
			out = append(out, p.Name())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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

// Handle decides, under the tenant lock and in this order: may the caller act on
// the subordinate ([authority.Actor.MayActOn]), then is the manager eligible
// (exists, no reporting cycle), then [authority.Actor.MayReplaceManager], and
// dispatches to [Membership.AssignManager]. Authority comes first so a caller
// who may not act on the subordinate learns nothing about the reporting chain.
// Self-management is rejected up front.
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
	err = h.w.locked(ctx, cmd.TenantID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor) error {
		return h.w.facts.memberships.UpdateByID(ctx, cmd.TenantID, subID, func(m *membership.Membership) (bool, error) {
			sub, err := h.w.facts.member(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			// Authority first: a caller who may not act on the subordinate learns
			// nothing about the proposed manager or the reporting chain.
			if err := actor.MayActOn(sub); err != nil {
				return false, err
			}
			mgrM, mgr, err := h.w.facts.memberByID(ctx, cmd.TenantID, mgrID, now)
			if errors.Is(err, membership.ErrNotFound) {
				return false, fmt.Errorf("%w: manager not found in tenant", authority.ErrManagerNotEligible)
			}
			if err != nil {
				return false, err
			}
			if err := h.w.facts.rejectManagerCycle(ctx, cmd.TenantID, mgrM, subID); err != nil {
				return false, err
			}
			cur, found, err := h.w.facts.currentManager(ctx, cmd.TenantID, m, now)
			if err != nil {
				return false, err
			}
			if err := actor.MayReplaceManager(sub, mgr, asCurrent(cur, found)); err != nil {
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
	err := h.w.updateMember(ctx, cmd.TenantID, cmd.MembershipID, cmd.Actor, now, func(ctx context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error {
		cur, found, err := h.w.facts.currentManager(ctx, cmd.TenantID, m, now)
		if err != nil {
			return err
		}
		if err := actor.MayClearManager(target, asCurrent(cur, found)); err != nil {
			return err
		}
		return m.RemoveManager(now)
	})
	return translateAuthorityWrite("remove_user_manager", err)
}
