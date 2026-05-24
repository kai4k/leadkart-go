package query

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// GetBatchQuery — single-batch read.
type GetBatchQuery struct {
	BatchID batch.ID
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
	return h.batches.GetByID(ctx, q.BatchID)
}

// ListBatchesByProductQuery — cursor-paginated batches list for a Product.
type ListBatchesByProductQuery struct {
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
	page, err := h.batches.ListByProductPage(ctx, q.ProductID, batch.ListFilter{
		IncludeExpired: q.IncludeExpired,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[*batch.Batch]{}, fmt.Errorf("list batches: %w", err)
	}
	return page, nil
}
