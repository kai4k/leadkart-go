package membership

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by Repository read methods when no Membership matches.
var ErrNotFound = errs.New(errs.KindNotFound, "membership", "membership not found")

// ErrAlreadyActive is returned when a caller attempts to create or reactivate
// a Membership for a Person that already has an Active Membership elsewhere.
//
// The DB partial unique index `WHERE status='Active' AND NOT is_deleted` is
// the authoritative enforcement; this typed error is what the adapter
// surfaces from the SQLSTATE 23505 constraint violation.
var ErrAlreadyActive = errs.New(errs.KindConflict, "membership", "person already has active membership")

// Repository persists Membership aggregates.
//
// Membership IS tenant-scoped — RLS policies on `identity.tenant_memberships`
// (per ADR 0006) restrict reads to the current tenant's membership rows.
// Cross-tenant lookups (login flow finding the Active Membership for a given
// PersonID) hit the auth_routing index per ADR 0006.
type Repository interface {
	// Add persists a brand-new Membership from [New]. Returns
	// [ErrAlreadyActive] if the PersonID already has an Active Membership.
	Add(ctx context.Context, m *Membership) error

	// UpdateByID loads, mutates via updateFn, persists, drains events.
	// Per ADR 0004 + TDL Sep 2024 UpdateFn pattern.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Membership) (bool, error)) error

	// GetByID returns the Membership or [ErrNotFound]. Tenant-scoped: returns
	// only Memberships visible under the current tenant context.
	GetByID(ctx context.Context, id ID) (*Membership, error)

	// GetActiveForPerson returns the (single) Active Membership for a Person
	// across all tenants. Used during login when resolving "which tenant
	// scope does this user belong to?".
	//
	// Implemented against the non-RLS auth_routing index — the only path
	// that legitimately reads across tenants. Returns [ErrNotFound] if the
	// Person has no Active Membership (treated as auth failure upstream).
	GetActiveForPerson(ctx context.Context, personID person.ID) (*Membership, error)

	// ListForTenant returns all Memberships under the current tenant scope.
	// Used by tenant admin UIs ("manage users").
	ListForTenant(ctx context.Context, tenantID tenant.ID) ([]*Membership, error)

	// ListAllForPerson returns every Membership (Active + Inactive) the
	// supplied Person holds across ALL tenants. Cross-tenant read —
	// platform-only path. Used by:
	//   - Platform operator "show all of this user's tenants" UI
	//   - Person-level cascades (anonymise, global suspend) where the
	//     handler needs to enumerate the affected memberships before
	//     dispatching per-tenant fanout events.
	//
	// Implementation: backed by the cross-tenant ListMembershipsForPerson
	// query which Postgres permits because the partial index on
	// (person_id) is non-RLS-filtered (per database.md "Single-Active-
	// Membership constraint"). Returns an empty slice (not ErrNotFound)
	// when the Person has no Memberships.
	ListAllForPerson(ctx context.Context, personID person.ID) ([]*Membership, error)
}
