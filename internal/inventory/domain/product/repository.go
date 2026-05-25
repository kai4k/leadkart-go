package product

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound is returned by Repository.GetByID / lookup methods when
// no matching live product exists in the caller's tenant scope.
var ErrNotFound = errs.New(errs.KindNotFound, "product", "product not found")

// ErrSKUTaken is returned by Repository.Add when another LIVE product
// in the same tenant already holds the SKU (case-insensitive — SKUs
// are canonicalised upper-case at the aggregate level).
var ErrSKUTaken = errs.New(errs.KindAlreadyExists, "product", "sku already taken in this tenant")

// ListFilter is the filter set the GET /v1/inventory/products endpoint
// surfaces. Per BRD §6.5 + ADR 0040 search canon:
//
//   - Search          — pg_trgm fuzzy match on (sku || name); empty = no filter.
//   - ActiveOnly      — exclude is_active = false rows when true.
//   - DosageForm      — exact match (case-insensitive); empty = no filter.
//   - Manufacturer    — exact match (case-insensitive); empty = no filter.
//
// SoftDeleted rows are NEVER returned through this filter — operators
// use a dedicated forensic endpoint (not in slice 1).
type ListFilter struct {
	Search       string
	ActiveOnly   bool
	DosageForm   string
	Manufacturer string
}

// Repository persists Product aggregates. Per Cheney "accept interfaces,
// return structs" — the consumer (application service) defines what it
// needs; adapters in `internal/inventory/adapters/` implement.
//
// Tenant scoping (ADR 0062 — TDL canon): every method that takes an ID
// without an aggregate ALSO takes an EXPLICIT tenantID parameter. The
// adapter binds the GUC from the parameter at tx-begin (NOT from ctx-
// tenancy.WithID — that's a domain value in context, which Khorikov §11
// + Cheney mark as a hidden input). RLS remains the security backstop;
// the explicit param is the API surface contract. Cross-tenant access
// surfaces as ErrNotFound per ADR 0044.
type Repository interface {
	// Add persists a brand-new product. The aggregate already carries
	// its TenantID — no separate param needed. Drains the aggregate's
	// events into the outbox same-tx per ADR 0008. Returns ErrSKUTaken
	// on unique-index violation.
	Add(ctx context.Context, p *Product) error

	// UpdateByID loads (scoped to tenantID) → updateFn → persist + emits
	// events under one tenant-scoped tx. Returns (true, nil) from
	// updateFn to commit; (false, nil) to abort; (_, err) rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID, updateFn func(*Product) (bool, error)) error

	// GetByID returns a LIVE (non-deleted) product in the supplied tenant
	// or ErrNotFound.
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Product, error)

	// ListPage returns a keyset-paginated page of LIVE products in the
	// supplied tenant scope, ordered (created_at DESC, id DESC) per
	// ADR 0038. Optionally filtered by [ListFilter].
	//
	// The cursor's SortValue is the row's CreatedAt; the tiebreaker is
	// the row's ID. Standard pagination.Page result shape.
	ListPage(ctx context.Context, tenantID tenant.ID, filter ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*Product], error)
}
