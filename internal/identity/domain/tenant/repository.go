package tenant

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// ErrNotFound is returned by Repository.GetByID / GetBySlug when no tenant
// exists for the supplied identifier.
var ErrNotFound = errs.New(errs.KindNotFound, "tenant", "tenant not found")

// ErrSlugTaken is returned by Repository.Add when the slug is already taken
// (DB unique constraint translated to this typed error).
var ErrSlugTaken = errs.New(errs.KindAlreadyExists, "tenant", "slug already taken")

// Repository persists Tenant aggregates. Declared in the domain package so the
// consumer (app layer) owns the contract; adapters in internal/identity/adapters/
// implement it (Cheney: "accept interfaces, return structs").
//
// All methods must be goroutine-safe.
//
// Tenant rows are NOT RLS-scoped (each row IS a tenant). Platform-operator
// queries bypass RLS via app.is_platform = true (ADR 0006).
type Repository interface {
	// Add persists a new Tenant created via [New]. Events are drained into the
	// outbox in the same transaction (ADR 0004 + ADR 0008).
	// Returns [ErrSlugTaken] on slug collision.
	Add(ctx context.Context, t *Tenant) error

	// UpdateByID loads the tenant, runs updateFn, then persists and emits events
	// in one transaction (TDL canon, ADR 0004). updateFn returns (true, nil) to
	// commit, (false, nil) to abort without changes, or (_, err) to roll back.
	// Returns [ErrNotFound] if the tenant doesn't exist.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Tenant) (bool, error)) error

	// GetByID returns the tenant or [ErrNotFound]. Read-only; no transaction.
	GetByID(ctx context.Context, id ID) (*Tenant, error)

	// GetBySlug returns the tenant by URL slug or [ErrNotFound].
	// Used for slug-availability checks and auth-routing.
	GetBySlug(ctx context.Context, s slug.Slug) (*Tenant, error)

	// ListAll returns every Tenant ordered by created_at. Platform-operator
	// path only; access gate lives in HTTP middleware.
	ListAll(ctx context.Context) ([]*Tenant, error)

	// HardDeleteRow physically removes the tenant row after the 30-day grace
	// window (data-retention.md). Callers must first run Tenant.HardDelete via
	// UpdateByID so the audit trail records the terminal event.
	// Idempotent: no-ops if the row is already gone.
	HardDeleteRow(ctx context.Context, id ID) error
}

// Compile-time check that sentinel errors are wrapped-comparable.
var _ = errors.Is
