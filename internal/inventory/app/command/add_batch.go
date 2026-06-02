package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// AddBatchCommand is the validated input for adding a Batch to a Product.
//
// TenantID is the caller's tenant scope; per ADR 0062 (TDL canon) it flows
// through an explicit field, never via context smuggling.
type AddBatchCommand struct {
	TenantID                   tenant.ID
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

// AddBatchHandler resolves the parent Product, constructs the Batch, and
// persists, all inside one UoW transaction.
//
// Per ADR 0061 amendment 1 (H4 race fix): running GetByID and Add as separate
// transactions left a window where the parent could be soft-deleted between
// load and insert, and the composite FK still accepted the orphan write.
// Wrapping both in uow.WithinTx closes it — load, re-check, and insert share
// one tx, so a concurrent SoftDelete serialises on the products row (MVCC).
type AddBatchHandler struct {
	uow        pg.UnitOfWork
	products   product.Repository
	batches    batch.Repository
	now        func() time.Time
	newBatchID func() batch.ID
}

// NewAddBatchHandler wires the handler. now is the injected clock (nil →
// time.Now). newBatchID is the aggregate-ID factory required by
// TestArch_HandlersInjectIDFactory; tests inject a deterministic one.
func NewAddBatchHandler(uow pg.UnitOfWork, products product.Repository, batches batch.Repository, now func() time.Time, newBatchID func() batch.ID) AddBatchHandler {
	if newBatchID == nil {
		panic("command: NewAddBatchHandler newBatchID required")
	}
	if now == nil {
		now = time.Now
	}
	return AddBatchHandler{uow: uow, products: products, batches: batches, now: now, newBatchID: newBatchID}
}

// Handle adds a batch to the product inside one tenant-scoped UoW. Returns
// product.ErrNotFound for a missing or soft-deleted parent (including the
// read-then-write race per ADR 0061 amendment 1), batch.ErrInvalid on spec
// failure, batch.ErrBatchNumberTaken on (product_id, batch_number) collision.
func (h AddBatchHandler) Handle(ctx context.Context, cmd AddBatchCommand) (AddBatchResult, error) {
	if cmd.TenantID.IsZero() {
		return AddBatchResult{}, errors.New("add_batch: tenant id required")
	}
	now := h.now()
	var result AddBatchResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		p, err := h.products.GetByID(ctx, cmd.TenantID, cmd.ProductID)
		if err != nil {
			if errors.Is(err, product.ErrNotFound) {
				return product.ErrNotFound
			}
			return fmt.Errorf("add batch: load product: %w", err)
		}
		// Defence-in-depth: GetByID already filters soft-deleted via the
		// LIVE-only query (see adapter), but the re-check guards against
		// future query changes + makes the invariant explicit at the
		// handler boundary.
		if p.IsDeleted() {
			return product.ErrNotFound
		}
		b, err := batch.New(
			h.newBatchID(),
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
			now,
		)
		if err != nil {
			return fmt.Errorf("add batch: construct: %w", err)
		}
		// Add joins the surrounding tx via pg.TxFromContext.
		if err := h.batches.Add(ctx, b); err != nil {
			return fmt.Errorf("add batch: persist: %w", err)
		}
		result = AddBatchResult{BatchID: b.ID()}
		return nil
	})
	if err != nil {
		return AddBatchResult{}, err
	}
	return result, nil
}
