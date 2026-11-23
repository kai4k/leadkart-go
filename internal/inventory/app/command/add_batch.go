package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// OpeningStockReason is the ledger reason of the inbound movement written
// when a batch is added together with its opening stock.
const OpeningStockReason = "Opening stock"

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
	// InitialQuantity is the opening stock received with the batch. 0 (the
	// zero value) adds an empty batch, as before. A positive value is applied
	// as an inbound movement in the same transaction as the batch insert.
	InitialQuantity int64
}

// AddBatchResult carries the new BatchID.
type AddBatchResult struct {
	BatchID batch.ID
	// Warnings names non-fatal conditions: see [WarningPastSellableCutoff].
	Warnings []string
}

// WarningPastSellableCutoff is returned (never an error) when stock is received
// into a batch already past the company sellable cut-off but not past its
// printed expiry: the units are on hand yet not for sale (they count in the
// unsellable bucket until set apart).
const WarningPastSellableCutoff = "past_sellable_cutoff"

// AddBatchHandler resolves the parent Product, constructs the Batch, and
// persists, all inside one UoW transaction.
//
// Per ADR 0061 amendment 1 (H4 race fix): running GetByID and Add as separate
// transactions left a window where the parent could be soft-deleted between
// load and insert, and the composite FK still accepted the orphan write.
// Wrapping both in uow.WithinTx closes it — load, re-check, and insert share
// one tx, so a concurrent SoftDelete serialises on the products row (MVCC).
//
// Opening stock (audit C6): with InitialQuantity > 0 the same tx also writes
// the inbound ledger row, so a batch and its first stock cannot half-exist.
// The inbound is applied to the new aggregate BEFORE the insert, so its
// refusals (an expired batch, a bad quantity) are decided before any write and
// the batch row is inserted already holding the stock.
type AddBatchHandler struct {
	uow           pg.UnitOfWork
	products      product.Repository
	batches       batch.Repository
	movements     stockmovement.Repository
	window        sellability.Resolver
	now           func() time.Time
	newBatchID    func() batch.ID
	newMovementID func() stockmovement.ID
}

// NewAddBatchHandler wires the handler. now is the injected clock (nil →
// time.Now). newBatchID and newMovementID are the aggregate-ID factories
// required by TestArch_HandlersInjectIDFactory; tests inject deterministic ones.
func NewAddBatchHandler(uow pg.UnitOfWork, products product.Repository, batches batch.Repository, movements stockmovement.Repository, window sellability.Resolver, now func() time.Time, newBatchID func() batch.ID, newMovementID func() stockmovement.ID) AddBatchHandler {
	if newBatchID == nil {
		panic("command: NewAddBatchHandler newBatchID required")
	}
	if newMovementID == nil {
		panic("command: NewAddBatchHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return AddBatchHandler{
		uow: uow, products: products, batches: batches, movements: movements, window: window,
		now: now, newBatchID: newBatchID, newMovementID: newMovementID,
	}
}

// Handle adds a batch to the product inside one tenant-scoped UoW. Returns
// product.ErrNotFound for a missing or soft-deleted parent (including the
// read-then-write race per ADR 0061 amendment 1), batch.ErrInvalid on spec
// failure or a negative InitialQuantity, batch.ErrExpired when opening stock
// is given for a batch that is already expired, batch.ErrBatchNumberTaken on
// (product_id, batch_number) collision. On any error nothing is written.
func (h AddBatchHandler) Handle(ctx context.Context, cmd AddBatchCommand) (AddBatchResult, error) {
	if cmd.TenantID.IsZero() {
		return AddBatchResult{}, errors.New("add_batch: tenant id required")
	}
	if cmd.InitialQuantity < 0 {
		return AddBatchResult{}, fmt.Errorf("%w: initial_quantity must be >= 0 (got %d)", batch.ErrInvalid, cmd.InitialQuantity)
	}
	// The company expiry rule (business day, cut-off) is read BEFORE the
	// transaction opens; ReadPolicy joins a caller transaction when there is one.
	w, err := h.window.Resolve(ctx, cmd.TenantID)
	if err != nil {
		return AddBatchResult{}, fmt.Errorf("add batch: %w", err)
	}
	if cmd.ManufactureDate.After(w.Today) {
		return AddBatchResult{}, fmt.Errorf("%w: manufacture_date %s", batch.ErrManufactureDateInFuture, cmd.ManufactureDate.Format(time.DateOnly))
	}
	now := h.now()
	var result AddBatchResult
	err = h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
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
		var opening *stockmovement.Movement
		if cmd.InitialQuantity > 0 {
			if opening, err = h.openingStock(b, cmd, now, w.Today); err != nil {
				return err
			}
		}
		// Add joins the surrounding tx via pg.TxFromContext.
		if err := h.batches.Add(ctx, b); err != nil {
			return fmt.Errorf("add batch: persist: %w", err)
		}
		if opening != nil {
			if err := h.movements.Add(ctx, opening); err != nil {
				return fmt.Errorf("add batch: persist opening stock: %w", err)
			}
		}
		result = AddBatchResult{BatchID: b.ID()}
		if cmd.InitialQuantity > 0 && !w.IsSellable(b.ExpiryDate()) {
			result.Warnings = []string{WarningPastSellableCutoff}
		}
		return nil
	})
	if err != nil {
		return AddBatchResult{}, err
	}
	return result, nil
}

// openingStock applies cmd.InitialQuantity (above 0) to the not-yet-stored
// batch as an inbound movement and returns its ledger row. It writes nothing:
// a refusal here leaves the database untouched.
func (h AddBatchHandler) openingStock(b *batch.Batch, cmd AddBatchCommand, now, today time.Time) (*stockmovement.Movement, error) {
	if err := b.ApplyMovement(batch.MovementInbound, cmd.InitialQuantity, now, today); err != nil {
		return nil, fmt.Errorf("add batch: opening stock: %w", err)
	}
	m, err := stockmovement.New(h.newMovementID(), stockmovement.Spec{
		BatchID:             b.ID(),
		ProductID:           b.ProductID(),
		TenantID:            b.TenantID(),
		Type:                batch.MovementInbound,
		Quantity:            cmd.InitialQuantity,
		QuantityOnHandAfter: b.QuantityOnHand(),
		Reason:              OpeningStockReason,
		ActorMembershipID:   cmd.ActorMembershipID,
	}, now)
	if err != nil {
		return nil, fmt.Errorf("add batch: opening stock: construct movement: %w", err)
	}
	return m, nil
}
