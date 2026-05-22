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
// Tenant scoping: roles are ITenantScoped — RLS filters reads/writes to
// the tenant bound on the connection's `app.tenant_id` GUC unless
// `app.is_platform=true` is set. Cross-tenant role enumeration (audit
// dashboards, support tooling) MUST run under platform context.
type Repository interface {
	// Add persists a brand-new Role created via [New]. The aggregate's
	// PullEvents are drained inside the same transaction and appended
	// to the outbox table per ADR 0004 + ADR 0008.
	//
	// Returns [ErrNameTaken] if a live Role with the same (tenant_id,
	// name) already exists. SQLSTATE 23505 from the partial unique
	// index is translated by the adapter.
	Add(ctx context.Context, r *Role) error

	// UpdateByID loads the role, runs updateFn (which mutates state via
	// aggregate methods), then persists + emits events — all in one
	// transaction. TDL Sep 2024 canon (ADR 0004).
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] if the role doesn't exist or is soft-deleted.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Role) (bool, error)) error

	// GetByID returns the role or [ErrNotFound]. Read-only path. Soft-
	// deleted roles are NOT returned (callers needing tombstones use
	// admin-tier audit queries that bypass this contract).
	GetByID(ctx context.Context, id ID) (*Role, error)

	// GetByTenantAndName returns the live role with the given name
	// in the given tenant, or [ErrNotFound]. Used by [DefaultRoleCatalog]
	// seeding (Task 19) for re-seed detection + admin "lookup-by-name".
	GetByTenantAndName(ctx context.Context, tenantID tenant.ID, name string) (*Role, error)

	// GetByIDs hydrates a batch of roles in one query. Used by the
	// PermissionResolver (Task 21) when computing a Membership's
	// effective permission set: given the Membership's RoleAssignments,
	// this returns the matching live roles. Soft-deleted roles are
	// silently dropped — a deleted role can no longer grant permissions.
	GetByIDs(ctx context.Context, ids []ID) ([]*Role, error)

	// ListByTenant returns every live role for the supplied tenant,
	// ordered by hierarchy_level, name. Used by admin role-management
	// UI + the DefaultRoleCatalog idempotency check.
	ListByTenant(ctx context.Context, tenantID tenant.ID) ([]*Role, error)

	// GetAncestors returns the ancestor chain of `id` (parent → grandparent
	// → … root). The role itself is NOT included; empty result = root role
	// (no parent). ADR 0054 — used by [Role.ChangeParent]'s cycle-detection
	// closure + by `SetRoleParentHandler`'s pre-validation pass to provide
	// the best ergonomic error message before the DB trigger fires.
	//
	// Soft-deleted ancestors are still returned (parent_role_id FK uses
	// ON DELETE SET NULL — a soft-deleted parent looks deleted but the
	// chain itself is intact until a hard delete; for the cycle check we
	// only care about set-membership, not liveness).
	GetAncestors(ctx context.Context, id ID) ([]*Role, error)
}

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
