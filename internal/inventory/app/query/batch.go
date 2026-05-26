package query

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// GetBatchQuery — single-batch read.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). Per ADR 0062 (TDL canon): tenantID flows through
// explicit query fields, not via context smuggling.
type GetBatchQuery struct {
	TenantID tenant.ID
	BatchID  batch.ID
}

// GetBatchHandler returns a Batch or batch.ErrNotFound.
type GetBatchHandler struct {
	batches batch.Repository
}

// NewGetBatchHandler wires the handler.
func NewGetBatchHandler(batches batch.Repository) GetBatchHandler {
	return GetBatchHandler{batches: batches}
}

// Handle returns the batch.
func (h GetBatchHandler) Handle(ctx context.Context, q GetBatchQuery) (*batch.Batch, error) {
	return h.batches.GetByID(ctx, q.TenantID, q.BatchID)
}

// ListBatchesByProductQuery — cursor-paginated batches list for a Product.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer).
type ListBatchesByProductQuery struct {
	TenantID       tenant.ID
	ProductID      product.ID
	Cursor         pagination.Cursor
	PageSize       int
	IncludeExpired bool
}

// ListBatchesByProductHandler returns a keyset-paginated page.
type ListBatchesByProductHandler struct {
	batches batch.Repository
}

// NewListBatchesByProductHandler wires the handler.
func NewListBatchesByProductHandler(batches batch.Repository) ListBatchesByProductHandler {
	return ListBatchesByProductHandler{batches: batches}
}

// Handle returns the page.
func (h ListBatchesByProductHandler) Handle(ctx context.Context, q ListBatchesByProductQuery) (pagination.Page[*batch.Batch], error) {
	page, err := h.batches.ListByProductPage(ctx, q.TenantID, q.ProductID, batch.ListFilter{
		IncludeExpired: q.IncludeExpired,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[*batch.Batch]{}, fmt.Errorf("list batches: %w", err)
	}
	return page, nil
}
