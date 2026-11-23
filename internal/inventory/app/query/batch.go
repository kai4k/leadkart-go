package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
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
	QuantityReserved           int64
	QuantityQuarantined        int64
	// QuantityAvailable and QuantityUnsellable are the batch free units split by
	// whether the batch is still sellable under the company expiry rule.
	QuantityAvailable  int64
	QuantityUnsellable int64
	// Sellable is the one expiry rule applied to this batch today; LastSellableDay
	// is the last day it may be sold; PastPrintedExpiry is the legal limit.
	Sellable          bool
	LastSellableDay   time.Time
	PastPrintedExpiry bool
	Version           int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// projectBatch maps the write aggregate to the flat read View — the
// single source of truth for batch read projection.
func projectBatch(b *batch.Batch, w sellability.Window) BatchView {
	bk := b.Buckets(w.IsSellable(b.ExpiryDate()))
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
		QuantityReserved:           b.QuantityReserved(),
		QuantityQuarantined:        b.QuantityQuarantined(),
		QuantityAvailable:          bk.Available,
		QuantityUnsellable:         bk.Unsellable,
		Sellable:                   w.IsSellable(b.ExpiryDate()),
		LastSellableDay:            w.LastSellableDay(b.ExpiryDate()),
		PastPrintedExpiry:          w.IsPastPrintedExpiry(b.ExpiryDate()),
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
	window  sellability.Resolver
}

// NewGetBatchHandler wires the handler.
func NewGetBatchHandler(batches batch.Repository, window sellability.Resolver) GetBatchHandler {
	return GetBatchHandler{batches: batches, window: window}
}

// Handle returns the batch View or batch.ErrNotFound.
func (h GetBatchHandler) Handle(ctx context.Context, q GetBatchQuery) (BatchView, error) {
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return BatchView{}, fmt.Errorf("get batch: %w", err)
	}
	b, err := h.batches.GetByID(ctx, q.TenantID, q.BatchID)
	if err != nil {
		return BatchView{}, err
	}
	return projectBatch(b, w), nil
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
	window  sellability.Resolver
}

// NewListBatchesByProductHandler wires the handler.
func NewListBatchesByProductHandler(batches batch.Repository, window sellability.Resolver) ListBatchesByProductHandler {
	return ListBatchesByProductHandler{batches: batches, window: window}
}

// Handle returns the page of BatchView.
func (h ListBatchesByProductHandler) Handle(ctx context.Context, q ListBatchesByProductQuery) (pagination.Page[BatchView], error) {
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return pagination.Page[BatchView]{}, fmt.Errorf("list batches: %w", err)
	}
	page, err := h.batches.ListByProductPage(ctx, q.TenantID, q.ProductID, batch.ListFilter{
		IncludeExpired: q.IncludeExpired,
		MinSellable:    w.MinSellable,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[BatchView]{}, fmt.Errorf("list batches: %w", err)
	}
	views := make([]BatchView, 0, len(page.Items))
	for _, b := range page.Items {
		views = append(views, projectBatch(b, w))
	}
	return pagination.Page[BatchView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}
