package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// BatchView is the flat read model for a single batch. Per STRICT CQRS
// (TDL canon) query handlers project the write aggregate into this read
// DTO; the [batch.Batch] aggregate NEVER leaks past the app layer into
// ports/. The port serializes this View into the wire BatchDto (1:1).
type BatchView struct {
	ID                         string
	ProductID                  string
	TenantID                   string
	BatchNumber                string
	ManufactureDate            time.Time
	ExpiryDate                 time.Time
	ManufacturerName           string
	ManufacturingLicenceNumber string
	MRPPaise                   int64
	PurchasePricePaise         int64
	QuantityOnHand             int64
	Version                    int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

// projectBatch maps the write aggregate to the flat read View — the
// single source of truth for batch read projection.
func projectBatch(b *batch.Batch) BatchView {
	return BatchView{
		ID:                         b.ID().String(),
		ProductID:                  b.ProductID().String(),
		TenantID:                   b.TenantID().String(),
		BatchNumber:                b.BatchNumber(),
		ManufactureDate:            b.ManufactureDate(),
		ExpiryDate:                 b.ExpiryDate(),
		ManufacturerName:           b.ManufacturerName(),
		ManufacturingLicenceNumber: b.ManufacturingLicenceNumber(),
		MRPPaise:                   b.MRPPaise(),
		PurchasePricePaise:         b.PurchasePricePaise(),
		QuantityOnHand:             b.QuantityOnHand(),
		Version:                    b.Version(),
		CreatedAt:                  b.CreatedAt(),
		UpdatedAt:                  b.UpdatedAt(),
	}
}

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

// Handle returns the batch View or batch.ErrNotFound.
func (h GetBatchHandler) Handle(ctx context.Context, q GetBatchQuery) (BatchView, error) {
	b, err := h.batches.GetByID(ctx, q.TenantID, q.BatchID)
	if err != nil {
		return BatchView{}, err
	}
	return projectBatch(b), nil
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

// Handle returns the page of BatchView.
func (h ListBatchesByProductHandler) Handle(ctx context.Context, q ListBatchesByProductQuery) (pagination.Page[BatchView], error) {
	page, err := h.batches.ListByProductPage(ctx, q.TenantID, q.ProductID, batch.ListFilter{
		IncludeExpired: q.IncludeExpired,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[BatchView]{}, fmt.Errorf("list batches: %w", err)
	}
	views := make([]BatchView, 0, len(page.Items))
	for _, b := range page.Items {
		views = append(views, projectBatch(b))
	}
	return pagination.Page[BatchView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}
