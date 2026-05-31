package product

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by [Repository.GetByID] and lookup methods when no matching live product exists.
var ErrNotFound = errs.New(errs.KindNotFound, "product", "product not found")

// ErrSKUTaken is returned by [Repository.Add] when another live product in the same tenant holds the SKU.
// SKUs are canonicalised upper-case; the check is case-insensitive.
var ErrSKUTaken = errs.New(errs.KindAlreadyExists, "product", "sku already taken in this tenant")

// ListFilter is the filter input for [Repository.ListPage] (GET /v1/inventory/products, BRD §6.5).
//
//   - Search       — pg_trgm fuzzy match on (sku || name); empty = no filter.
//   - ActiveOnly   — when true, exclude is_active = false rows.
//   - DosageForm   — exact match (case-insensitive); empty = no filter.
//   - Manufacturer — exact match (case-insensitive); empty = no filter.
//
// Soft-deleted rows are never returned; use a dedicated forensic endpoint for those.
type ListFilter struct {
	Search       string
	ActiveOnly   bool
	DosageForm   string
	Manufacturer string
}

// Repository persists Product aggregates.
// Defined by the consumer (application layer); implemented in internal/inventory/adapters/.
//
// Tenant scoping (ADR 0062): every lookup method takes an explicit tenantID — not ctx-injected tenancy
// (Khorikov §11 + Cheney: hidden input). The adapter binds the GUC at tx-begin.
// RLS is the security backstop; cross-tenant access surfaces as [ErrNotFound] (ADR 0044).
type Repository interface {
	// Add persists a new product and drains its events into the outbox in the same tx (ADR 0008).
	// TenantID is read from the aggregate. Returns [ErrSKUTaken] on unique-index violation.
	Add(ctx context.Context, p *Product) error

	// UpdateByID loads (scoped to tenantID), calls updateFn, and persists + emits events in one tx.
	// updateFn returns (true, nil) to commit, (false, nil) to abort, (_, err) to roll back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Product) (bool, error)) error

	// GetByID returns a live (non-deleted) product in the supplied tenant scope or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Product, error)

	// ListPage returns a keyset-paginated page of live products in tenant scope,
	// ordered (created_at DESC, id DESC) per ADR 0038, filtered by [ListFilter].
	// Cursor.SortValue = CreatedAt; tiebreaker = ID.
	ListPage(ctx context.Context, tenantID tenant.ID, filter ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*Product], error)
}
