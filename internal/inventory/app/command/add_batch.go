package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// AddBatchCommand carries the validated input for adding a new Batch to
// a Product. Defensive read of the parent Product up front to surface
// 404 cleanly (the composite-FK would otherwise return a generic FK
// violation).
type AddBatchCommand struct {
	ProductID                  product.ID
	ActorMembershipID          membership.ID
	BatchNumber                string
	ManufactureDate            time.Time
	ExpiryDate                 time.Time
	ManufacturerName           string
	ManufacturingLicenceNumber string
	MRPPaise                   int64
	PurchasePricePaise         int64
}

// AddBatchResult carries the new BatchID.
type AddBatchResult struct {
	BatchID batch.ID
}

// AddBatchHandler — resolves parent Product, constructs Batch, persists.
//
// Pre-tx product GET prevents the unfriendly FK-violation that would
// otherwise surface on a stray product id. The Add path is single-tx
// (Batch insert + outbox event in the batches adapter).
type AddBatchHandler struct {
	products product.Repository
	batches  batch.Repository
}

// NewAddBatchHandler wires the handler.
func NewAddBatchHandler(products product.Repository, batches batch.Repository) AddBatchHandler {
	return AddBatchHandler{products: products, batches: batches}
}

// Handle adds a batch to the given product.
//
// Returns product.ErrNotFound if the parent doesn't exist (or is
// soft-deleted) in the caller's tenant scope.
// Returns batch.ErrInvalid (wrapped) on spec failure.
// Returns batch.ErrBatchNumberTaken on (product_id, batch_number)
// unique-violation.
func (h AddBatchHandler) Handle(ctx context.Context, cmd AddBatchCommand) (AddBatchResult, error) {
	p, err := h.products.GetByID(ctx, cmd.ProductID)
	if err != nil {
		if errors.Is(err, product.ErrNotFound) {
			return AddBatchResult{}, product.ErrNotFound
		}
		return AddBatchResult{}, fmt.Errorf("add batch: load product: %w", err)
	}
	if p.IsDeleted() {
		return AddBatchResult{}, product.ErrNotFound
	}
	b, err := batch.New(
		batch.ID(ids.NewV7().String()),
		p.ID(),
		p.TenantID(),
		cmd.ActorMembershipID,
		batch.Spec{
			BatchNumber:                cmd.BatchNumber,
			ManufactureDate:            cmd.ManufactureDate,
			ExpiryDate:                 cmd.ExpiryDate,
			ManufacturerName:           cmd.ManufacturerName,
			ManufacturingLicenceNumber: cmd.ManufacturingLicenceNumber,
			MRPPaise:                   cmd.MRPPaise,
			PurchasePricePaise:         cmd.PurchasePricePaise,
		},
	)
	if err != nil {
		return AddBatchResult{}, fmt.Errorf("add batch: construct: %w", err)
	}
	if err := h.batches.Add(ctx, b); err != nil {
		return AddBatchResult{}, fmt.Errorf("add batch: persist: %w", err)
	}
	return AddBatchResult{BatchID: b.ID()}, nil
}
