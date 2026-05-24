package batch

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ErrNotFound — no LIVE batch matched the supplied ID / lookup criteria
// in the caller's tenant scope.
var ErrNotFound = errs.New(errs.KindNotFound, "batch", "batch not found")

// ErrBatchNumberTaken — adapter surfaces this on the partial-unique-
// index violation for (product_id, batch_number) WHERE NOT is_deleted.
var ErrBatchNumberTaken = errs.New(errs.KindAlreadyExists, "batch", "batch_number already exists for this product")

// ErrAnyLiveStock — application-layer guard: cannot soft-delete the
// parent product when any of its live batches still hold stock. Lives
// here so the application + adapter share the sentinel.
var ErrAnyLiveStock = errs.New(errs.KindConflict, "batch", "product has live batches with non-zero stock")

// ListFilter is the filter set the GET /v1/inventory/products/{id}/batches
// endpoint surfaces. SoftDeleted batches are always excluded.
type ListFilter struct {
	// IncludeExpired when true returns expired batches alongside live
	// ones; default (false) filters them out at SQL level.
	IncludeExpired bool
}

// Repository persists Batch aggregates.
type Repository interface {
	// Add persists a brand-new batch. Drains aggregate events into the
	// outbox same-tx. Returns ErrBatchNumberTaken on unique-index
	// violation; ErrNotFound if the parent product_id doesn't exist
	// in the tenant scope (FK violation surfaced friendly).
	Add(ctx context.Context, b *Batch) error

	// UpdateByID loads → updateFn → persist + emits events under one
	// tenant-scoped tx with optimistic-concurrency check (WHERE version=$).
	// Returns ErrConcurrencyConflict on row-not-found-by-version; callers
	// (the command handler) implement a small retry loop.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Batch) (bool, error)) error

	// GetByID returns a LIVE batch or ErrNotFound.
	GetByID(ctx context.Context, id ID) (*Batch, error)

	// ListByProductPage returns batches for the supplied product,
	// ordered (expiry_date ASC, id DESC) — FEFO ordering per BRD §6.5.
	ListByProductPage(ctx context.Context, productID product.ID, filter ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*Batch], error)

	// AnyLiveWithStockForProduct reports whether any LIVE batch with
	// quantity_on_hand > 0 exists for productID in the tenant scope.
	// Used by the DeleteProductHandler to enforce the "no live stock"
	// guard.
	AnyLiveWithStockForProduct(ctx context.Context, productID product.ID) (bool, error)
}
