package tenant

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// ----- Sentinel errors ------------------------------------------------------

// ErrNotFound is returned by Repository.GetByID / GetBySlug when no tenant
// exists for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "tenant", "tenant not found")

// ErrSlugTaken is returned by Repository.Add when a tenant with the same
// slug already exists. (DB unique constraint surfaces as this typed error.)
var ErrSlugTaken = errs.New(errs.KindAlreadyExists, "tenant", "slug already taken")

// ----- Repository contract --------------------------------------------------

// Repository persists Tenant aggregates. The contract is declared here in
// the domain package per Cheney "accept interfaces, return structs" — the
// CONSUMER (the application service) defines what it needs; adapters in
// internal/identity/adapters/ implement.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
//
// All methods MUST honour [tenancy.FromContext] for tenant-scoped reads:
// pgxpool's AfterAcquire callback issues `SET LOCAL app.tenant_id = $1`
// per transaction (ADR 0006), then Postgres RLS does the filtering.
//
// Note: Tenant itself is NOT tenant-scoped (each row IS a tenant). RLS
// applies only to platform-operator queries that span tenants — the
// platform-operator role bypasses RLS via `app.is_platform = true`.
type Repository interface {
	// Add persists a brand-new tenant created via [New]. The aggregate's
	// PullEvents are drained inside the same transaction and appended to
	// the outbox table per ADR 0004 + ADR 0008.
	//
	// Returns [ErrSlugTaken] if a tenant with the same slug already exists.
	Add(ctx context.Context, t *Tenant) error

	// UpdateByID loads the tenant, runs updateFn (which mutates state via
	// aggregate methods), then persists + emits events — all in one
	// transaction. TDL Sep 2024 canon (ADR 0004).
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	//
	// Returns [ErrNotFound] if the tenant doesn't exist.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Tenant) (bool, error)) error

	// GetByID returns the tenant or [ErrNotFound]. Read-only path; does
	// not drain events or open a transaction.
	GetByID(ctx context.Context, id ID) (*Tenant, error)

	// GetBySlug returns the tenant by URL slug or [ErrNotFound].
	// Used during signup for slug-availability checks + login flows
	// where the slug appears in the auth-routing index.
	GetBySlug(ctx context.Context, s slug.Slug) (*Tenant, error)
}

// Compile-time guarantee that the sentinel errors are wrapped-comparable.
var _ = errors.Is
