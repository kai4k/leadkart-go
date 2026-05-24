package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
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

// AddBatchHandler — resolves parent Product, constructs Batch, persists
// — ALL inside ONE UoW transaction.
//
// Per ADR 0061 amendment 1 (H4 race fix): the prior shape ran
// `products.GetByID` and `batches.Add` as TWO separate transactions,
// leaving a window where the parent product could be soft-deleted
// between the load and the batch insert. The composite FK
// `(product_id, tenant_id) → products(id, tenant_id)` is satisfied by
// the soft-deleted row's PK so the orphan write succeeded — yielding a
// live batch hanging off a deleted product.
//
// Fix: wrap both in `uow.WithinTx`. The load runs inside the tx; the
// re-check `p.IsDeleted()` runs against the snapshot the same tx will
// see; the batch insert joins the tx via `pg.TxFromContext`. Any
// concurrent SoftDelete that committed BEFORE this tx started is
// visible; any concurrent SoftDelete in flight ALONGSIDE this tx serialises
// on the products row's xmin (Postgres MVCC — the loser surfaces as a
// fresh GetByID returning the deleted row on its retry).
type AddBatchHandler struct {
	uow      pg.UnitOfWork
	products product.Repository
	batches  batch.Repository
	now      func() time.Time
}

// NewAddBatchHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor — composition root wires
// `time.Now`; tests inject a fixed-time closure. Nil → time.Now.
func NewAddBatchHandler(uow pg.UnitOfWork, products product.Repository, batches batch.Repository, now func() time.Time) AddBatchHandler {
	if now == nil {
		now = time.Now
	}
	return AddBatchHandler{uow: uow, products: products, batches: batches, now: now}
}

// Handle adds a batch to the given product inside one tenant-scoped UoW.
//
// Returns product.ErrNotFound if the parent doesn't exist (or is
// soft-deleted) in the caller's tenant scope — including the
// soft-deleted-between-read-and-write race per ADR 0061 amendment 1.
// Returns batch.ErrInvalid (wrapped) on spec failure.
// Returns batch.ErrBatchNumberTaken on (product_id, batch_number)
// unique-violation.
func (h AddBatchHandler) Handle(ctx context.Context, cmd AddBatchCommand) (AddBatchResult, error) {
	now := h.now()
	var result AddBatchResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		p, err := h.products.GetByID(ctx, cmd.ProductID)
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
