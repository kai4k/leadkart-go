package role

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by [Repository.GetByID] / [Repository.GetByTenantAndName]
// when no row of the tenant matches the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "role", "role not found")

// ErrNameTaken is returned by [Repository.Add] when a Role with the
// same (tenant_id, name) already exists, inactive or not. The DB-level unique
// index `uq_roles_tenant_name` raises 23505;
// repos translate via [pg.SQLStateUniqueViolation] mapping.
var ErrNameTaken = errs.New(errs.KindAlreadyExists, "role", "role name already taken in this tenant")

// ----- Repository contract --------------------------------------------------

// Repository persists [Role] aggregates. The contract is declared here in
// the domain package per Cheney "accept interfaces, return structs" — the
// CONSUMER (the application service) defines what it needs; adapters in
// internal/identity/adapters/ implement.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input). RLS remains the security backstop;
// the explicit param is the API surface contract.
type Repository interface {
	// Add persists a brand-new Role created via [New]. The aggregate
	// already carries its TenantID — no separate param needed. The
	// aggregate's PullEvents are drained inside the same transaction
	// and appended to the outbox table per ADR 0004 + ADR 0008.
	//
	// Returns [ErrNameTaken] if a Role (active or inactive) with the same (tenant_id,
	// name) already exists. SQLSTATE 23505 from the partial unique
	// index is translated by the adapter.
	Add(ctx context.Context, r *Role) error

	// UpdateByID loads the role (scoped to tenantID), runs updateFn,
	// then persists + emits events — all in one transaction. TDL Sep
	// 2024 canon (ADR 0004).
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] if the role doesn't exist in the tenant. An INACTIVE
	// role is loaded too (it can be reactivated; its mutators refuse, ADR 0073).
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Role) (bool, error)) error

	// GetByID returns the role from the supplied tenant, active or inactive,
	// or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Role, error)

	// GetByTenantAndName returns the role (any status) with the given name
	// in the given tenant, or [ErrNotFound]. Used by [DefaultRoleCatalog]
	// seeding (Task 19) for re-seed detection + admin "lookup-by-name".
	GetByTenantAndName(ctx context.Context, tenantID tenant.ID, name string) (*Role, error)

	// GetByIDs hydrates a batch of roles in one query, scoped to the
	// supplied tenant. Used by the PermissionResolver when computing a
	// Membership's effective permission set. INACTIVE roles + IDs
	// outside the tenant are silently dropped: an inactive role confers nothing.
	GetByIDs(ctx context.Context, tenantID tenant.ID, ids []ID) ([]*Role, error)

	// GetByIDsIncludingInactive is GetByIDs without the active filter.
	// Authority decisions only (ADR 0069): a membership's assigned roles must
	// be loaded COMPLETE, inactive ones included, because an inactive role
	// confers nothing but a partial set would make the member look weaker
	// than it is. IDs outside the tenant are still silently dropped.
	GetByIDsIncludingInactive(ctx context.Context, tenantID tenant.ID, ids []ID) ([]*Role, error)

	// ListByTenant returns every role (active and inactive) for the supplied tenant,
	// ordered by hierarchy_level, name.
	ListByTenant(ctx context.Context, tenantID tenant.ID) ([]*Role, error)
}

// Note: GetAncestors retired in Wave 9.4 (ADR 0058). The
// rolehierarchy.Repository.GetAncestorsByChild method now owns
// ancestor walks against the dedicated edges table.

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
