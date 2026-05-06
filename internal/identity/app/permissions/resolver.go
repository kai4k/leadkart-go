// Package permissions hosts the [Resolver] application service —
// the canonical "what permissions does THIS Membership effectively
// hold" computation. Single source of truth consumed by:
//
//   - Login + Refresh handlers (Task 22) — emit the resolved set as
//     repeated `permission` JWT claims at issuance.
//   - RequirePermission HTTP middleware (Task 23) — verifies a
//     claim against the requested permission name.
//   - Future admin "view a user's effective permissions" UI.
//
// The resolution rule lives in the [membership.Membership] aggregate
// (`Membership.EffectivePermissions(roles)`); this service only
// orchestrates the cross-aggregate load (Membership + its assigned
// Roles) so the aggregate can compute the set under set semantics
// per `multi-tenancy.md` "Identity model — Person + TenantMembership"
// + .NET parent's `IdentityPermissionResolver`.
package permissions

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Resolver computes a Membership's effective permission set. Pure
// orchestrator — no caching here. The caller (login/refresh handler
// or middleware) is the cache facade boundary per
// `coding-standards.md` "Cache facade per concern" + future Task
// 21+ ISecurityStampCache analogue.
type Resolver struct {
	memberships membership.Repository
	roles       role.Repository
}

// NewResolver wires the orchestrator. Both deps are interfaces (the
// domain-layer Repository contracts) so tests + cache decorators can
// substitute fakes without reaching into infrastructure.
func NewResolver(memberships membership.Repository, roles role.Repository) *Resolver {
	return &Resolver{memberships: memberships, roles: roles}
}

// Resolve returns the Membership's effective permission set per
// `Membership.EffectivePermissions`:
//
//	union(role.Permissions for r in m.RoleAssignments())
//	  ∪ m.GrantedPermissions
//	  \ m.RevokedPermissions
//
// The empty input case (Membership with no roles AND no overlay
// grants) yields an empty slice with no error.
//
// Soft-deleted roles drop silently — `RoleRepository.GetByIDs` filters
// them out by contract; a deleted role can no longer grant authority.
//
// Tenant context: caller must have set tenancy.WithID(ctx, ...) before
// invoking. Both repo lookups run under the bound tenant scope per
// `multi-tenancy.md` RLS canon.
func (r *Resolver) Resolve(
	ctx context.Context,
	membershipID membership.ID,
) ([]*permission.Permission, error) {
	m, err := r.memberships.GetByID(ctx, membershipID)
	if err != nil {
		return nil, fmt.Errorf("permissions: load membership %q: %w", membershipID, err)
	}
	roles, err := r.loadRolesForMembership(ctx, m.RoleAssignments())
	if err != nil {
		return nil, err
	}
	return m.EffectivePermissions(roles), nil
}

// ResolveForLoaded skips the membership load — use this when the
// caller already has the [membership.Membership] aggregate in hand
// (login flow loads it for SecurityStamp + status checks; no point
// re-fetching).
//
// Returns the same shape as [Resolver.Resolve].
func (r *Resolver) ResolveForLoaded(
	ctx context.Context,
	m *membership.Membership,
) ([]*permission.Permission, error) {
	if m == nil {
		return nil, errors.New("permissions: membership required")
	}
	roles, err := r.loadRolesForMembership(ctx, m.RoleAssignments())
	if err != nil {
		return nil, err
	}
	return m.EffectivePermissions(roles), nil
}

// AuthClaims is the bundled output of [Resolver.ResolveAuth] — the two
// values JWT-issuance paths need together: the effective permission set
// + the SuperUser flag (true iff any of the Membership's RoleAssignments
// references a Role with `IsSuperAdmin == true`).
//
// Loading the Roles once + folding both computations is cheaper than the
// caller invoking Resolve + a parallel "is super-admin?" query.
type AuthClaims struct {
	Permissions []*permission.Permission
	IsSuperUser bool
}

// ResolveAuth computes [AuthClaims] for the supplied loaded Membership.
// Used by Login + Refresh handlers at JWT issuance — they already hold
// the Membership aggregate and don't need a second Repository round-trip.
//
// IsSuperUser drives the JWT `is_super_user` claim per `multi-tenancy.md`
// "SuperUser god-mode" — the runtime authorization-check short-circuit.
// Computed once at issuance; never re-checked per request.
func (r *Resolver) ResolveAuth(
	ctx context.Context,
	m *membership.Membership,
) (AuthClaims, error) {
	if m == nil {
		return AuthClaims{}, errors.New("permissions: membership required")
	}
	roles, err := r.loadRolesForMembership(ctx, m.RoleAssignments())
	if err != nil {
		return AuthClaims{}, err
	}
	isSuper := false
	for _, rl := range roles {
		if rl.IsSuperAdmin() {
			isSuper = true
			break
		}
	}
	return AuthClaims{
		Permissions: m.EffectivePermissions(roles),
		IsSuperUser: isSuper,
	}, nil
}

func (r *Resolver) loadRolesForMembership(
	ctx context.Context,
	ids []role.ID,
) ([]*role.Role, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	loaded, err := r.roles.GetByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("permissions: load roles: %w", err)
	}
	return loaded, nil
}
