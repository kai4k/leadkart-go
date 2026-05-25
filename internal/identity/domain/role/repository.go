package role

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by [Repository.GetByID] / [Repository.GetByTenantAndName]
// when no live (non-deleted) row matches the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "role", "role not found")

// ErrNameTaken is returned by [Repository.Add] when a Role with the
// same (tenant_id, name) already exists. The DB-level partial unique
// index `uq_roles_tenant_name WHERE NOT is_deleted` raises 23505;
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
	// Returns [ErrNameTaken] if a live Role with the same (tenant_id,
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
	// Returns [ErrNotFound] if the role doesn't exist in the tenant
	// or is soft-deleted.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Role) (bool, error)) error

	// GetByID returns the role from the supplied tenant or [ErrNotFound].
	// Soft-deleted roles are NOT returned.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Role, error)

	// GetByTenantAndName returns the live role with the given name
	// in the given tenant, or [ErrNotFound]. Used by [DefaultRoleCatalog]
	// seeding (Task 19) for re-seed detection + admin "lookup-by-name".
	GetByTenantAndName(ctx context.Context, tenantID tenant.ID, name string) (*Role, error)

	// GetByIDs hydrates a batch of roles in one query, scoped to the
	// supplied tenant. Used by the PermissionResolver when computing a
	// Membership's effective permission set. Soft-deleted roles + IDs
	// outside the tenant are silently dropped.
	GetByIDs(ctx context.Context, tenantID tenant.ID, ids []ID) ([]*Role, error)

	// ListByTenant returns every live role for the supplied tenant,
	// ordered by hierarchy_level, name.
	ListByTenant(ctx context.Context, tenantID tenant.ID) ([]*Role, error)
}

// Note: GetAncestors retired in Wave 9.4 (ADR 0058). The
// rolehierarchy.Repository.GetAncestorsByChild method now owns
// ancestor walks against the dedicated edges table.

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
