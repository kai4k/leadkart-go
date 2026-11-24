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

// MaxRoleInUseHolderNames is how many holder names a role_in_use refusal carries.
const MaxRoleInUseHolderNames = 5

// ErrRoleInUse surfaces when a delete is refused because a membership, active or inactive,
// still holds the role (owner ruling 2026-11-25: a role is deleted only when nobody holds it).
// Wire shape: 409 role_in_use with the holder count and names (see [RoleInUseError]).
var ErrRoleInUse = errors.New("role: still held by members")

// RoleInUseError is [ErrRoleInUse] with every holder counted (Holders), how many of them are
// active (ActiveHolders), and the display names of up to [MaxRoleInUseHolderNames] of them.
type RoleInUseError struct {
	Holders       int
	ActiveHolders int
	HolderNames   []string
}

func (e *RoleInUseError) Error() string {
	return fmt.Sprintf("%s: %d holders (%d active)", ErrRoleInUse, e.Holders, e.ActiveHolders)
}

// Is makes errors.Is(err, ErrRoleInUse) match.
func (e *RoleInUseError) Is(target error) bool { return target == ErrRoleInUse }

// roleWriter is the shared base of the role handlers: the tenant lock, the
// facts loader and one clock.
type roleWriter struct{ authorityWriter }

// holderScope says which of a role's holders an edit loads.
type holderScope int

const (
	// holdersNone: the edit does not act on holders (a rename).
	holdersNone holderScope = iota
	// holdersOnly: the holders (permission changes, deactivate, reactivate).
	holdersOnly
	// holdersAndReporting: the holders and the members linked to them by
	// reports_to (a re-level).
	holdersAndReporting
)

// roleDefinition is what a role confers and where it stands: the part of a role
// whose change reaches every holder's tokens.
type roleDefinition struct {
	level    int
	perms    string
	inactive bool
}

func definitionOf(r *role.Role) roleDefinition {
	names := authority.PermissionNames(r.Permissions())
	slices.Sort(names)
	return roleDefinition{level: r.HierarchyLevel(), perms: strings.Join(names, ","), inactive: !r.IsActive()}
}

// updateRole runs fn (decide, then mutate) on the role under the lock; a
// foreign role is role.ErrNotFound (an inactive role is loaded: reactivating it is an edit). It is the one path every
// role-definition edit takes, so it is also the one place the holders are
// loaded (once, per scope) and the one place their stamps end: when fn changed
// what the role confers, its level or its active state, every ACTIVE holder's
// security stamp is rotated in the same transaction, so their tokens carrying the
// old permissions and rank answer 401 stale_token on the next request in any
// module. A rename-only edit loads no holders and rotates nothing. Cost: the
// holders' reads are fixed ([authorityFacts.roleHolders]); the rotation is one
// person write per active holder, which the per-person security-stamp event
// requires.
func (w roleWriter) updateRole(ctx context.Context, tenantID tenant.ID, id role.ID, actor authority.Actor, now time.Time, scope holderScope,
	fn func(actor authority.Actor, r *role.Role, holders loadedHolders) error) error {
	return w.locked(ctx, tenantID, actor, now, func(ctx context.Context, actor authority.Actor) error {
		return w.facts.roles.UpdateByID(ctx, tenantID, id, func(r *role.Role) (bool, error) {
			var holders loadedHolders
			if scope != holdersNone {
				var err error
				if holders, err = w.facts.roleHolders(ctx, tenantID, id, now, scope == holdersAndReporting); err != nil {
					return false, err
				}
			}
			before := definitionOf(r)
			if err := fn(actor, r, holders); err != nil {
				return false, err
			}
			if scope != holdersNone && definitionOf(r) != before {
				for _, m := range holders.memberships {
					if m.Status() != membership.StatusActive {
						continue
					}
					if err := w.rotateStamp(ctx, m.PersonID(), "role_definition_changed", now); err != nil {
						return false, err
					}
				}
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
	scope := holdersNone
	if newLevel != authority.NoLevelChange {
		scope = holdersAndReporting
	}
	err := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, scope, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		if err := actor.MayEditRole(r, newLevel, holders.set); err != nil {
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
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, holdersOnly, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		if err := actor.MayReplaceRolePermissions(r, authority.PermissionNames(target), holders.set); err != nil {
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
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, holdersOnly, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		if err := actor.MayGrantRolePermission(r, p.Name(), holders.set); err != nil {
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
	upErr := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, holdersOnly, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		if err := actor.MayRevokeRolePermission(r, p.Name(), holders.set); err != nil {
			return err
		}
		return r.RevokePermission(p, now)
	})
	return translateAuthorityWrite("revoke_role_permission", upErr)
}

// ----- Deactivate / Reactivate role (ADR 0073: a role is never deleted) -----

// ChangeRoleStatusCommand moves a role between active and inactive. ChangedBy is the caller's
// PersonID (from the JWT Subject claim), recorded for audit.
type ChangeRoleStatusCommand struct {
	TenantID  tenant.ID
	RoleID    role.ID
	ChangedBy string
	// Actor is built once by the port via authn.AuthorityFrom; the zero value denies.
	Actor authority.Actor
}

// DeactivateRoleHandler runs the deactivate flow.
type DeactivateRoleHandler struct{ roleWriter }

// NewDeactivateRoleHandler wires the handler.
func NewDeactivateRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) DeactivateRoleHandler {
	return DeactivateRoleHandler{roleWriter{newAuthorityWriter("NewDeactivateRoleHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayDeactivateRole] (authority first), then applies the
// role-in-use rule (owner ruling 2026-11-25, ADR 0073 addendum): ANY holder, active or inactive,
// refuses it ([RoleInUseError], nothing changes), so a role is deleted only once nobody holds it.
// Otherwise the role goes inactive. An inactive role confers nothing (every authority read skips
// it). Reactivating gives it back to every holder (and the actor needed authority over every
// holder to do it). An inactive role is a no-op (idempotent).
func (h DeactivateRoleHandler) Handle(ctx context.Context, cmd ChangeRoleStatusCommand) error {
	if err := validateRoleCommand("deactivate_role", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	now := h.now()
	err := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, holdersOnly, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		if err := actor.MayDeactivateRole(r, holders.set); err != nil {
			return err
		}
		if !r.IsActive() {
			return nil
		}
		if len(holders.memberships) > 0 {
			return h.inUse(ctx, holders)
		}
		return r.Deactivate(cmd.ChangedBy, now)
	})
	return translateAuthorityWrite("deactivate_role", err)
}

// inUse builds the refusal: every holder counted, active ones apart, and the names of the first few.
func (h DeactivateRoleHandler) inUse(ctx context.Context, holders loadedHolders) error {
	out := &RoleInUseError{Holders: len(holders.memberships)}
	var personIDs []person.ID
	for _, m := range holders.memberships {
		if m.Status() == membership.StatusActive {
			out.ActiveHolders++
		}
		if len(personIDs) < MaxRoleInUseHolderNames {
			personIDs = append(personIDs, m.PersonID())
		}
	}
	people, err := h.facts.persons.GetByIDs(ctx, personIDs)
	if err != nil {
		return fmt.Errorf("deactivate_role: load holder names: %w", err)
	}
	for _, id := range personIDs {
		if p, ok := people[id]; ok {
			out.HolderNames = append(out.HolderNames, strings.TrimSpace(p.FirstName()+" "+p.LastName()))
		}
	}
	return out
}

// ReactivateRoleHandler runs the reactivate flow.
type ReactivateRoleHandler struct{ roleWriter }

// NewReactivateRoleHandler wires the handler.
func NewReactivateRoleHandler(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ReactivateRoleHandler {
	return ReactivateRoleHandler{roleWriter{newAuthorityWriter("NewReactivateRoleHandler", uow, lock, m, r, p, now)}}
}

// Handle decides [authority.Actor.MayReactivateRole] over the role and every holder it has
// (reactivating gives the role's permissions back to all of them: it needs the authority of
// deactivating AND the durable hold every conferral needs), then makes the role active. Every ACTIVE holder's security stamp rotates in the
// same transaction (the role definition changed), so their tokens are refreshed. An active role is
// a no-op (idempotent).
func (h ReactivateRoleHandler) Handle(ctx context.Context, cmd ChangeRoleStatusCommand) error {
	if err := validateRoleCommand("reactivate_role", cmd.TenantID, cmd.RoleID, true, cmd.Actor); err != nil {
		return err
	}
	now := h.now()
	err := h.updateRole(ctx, cmd.TenantID, cmd.RoleID, cmd.Actor, now, holdersOnly, func(actor authority.Actor, r *role.Role, holders loadedHolders) error {
		decide := actor.MayReactivateRole
		if r.IsActive() {
			decide = actor.MayDeactivateRole // nothing is conferred by a no-op
		}
		if err := decide(r, holders.set); err != nil {
			return err
		}
		// A name is unique among ACTIVE roles only (ADR 0073 addendum): if a newer role took it while
		// this one was inactive, the repository refuses the write with [role.ErrNameTaken] (the partial
		// unique index decides, race-free), which becomes 409 role_name_taken.
		return r.Activate(cmd.ChangedBy, now)
	})
	return translateAuthorityWrite("reactivate_role", err)
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
		if !child.IsActive() {
			return role.ErrInactive
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
