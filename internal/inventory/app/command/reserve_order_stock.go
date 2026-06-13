package command

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// ReserveLine is one product + quantity to reserve for a confirmed order.
type ReserveLine struct {
	ProductID product.ID
	Quantity  int64
}

// ReserveOrderStockCommand soft-commits FEFO stock to a confirmed order under
// the true-hold model (ADR 0063 §4). OrderID is the opaque cross-module order
// id used as the ledger source_reference.
type ReserveOrderStockCommand struct {
	TenantID          tenant.ID
	OrderID           string
	ActorMembershipID membership.ID
	Lines             []ReserveLine
}

// ReserveOrderStockResult reports the outcome. Reserved is true only when ALL
// lines were fully reserved; AlreadyReserved signals an idempotent no-op (the
// order was reserved by a prior delivery); Shortfalls is non-empty when stock
// was insufficient (nothing reserved — a StockReservationFailedV1 was emitted).
type ReserveOrderStockResult struct {
	Reserved        bool
	AlreadyReserved bool
	Shortfalls      []integrationevents.ReservationShortfall
}

// ReserveOrderStockHandler reserves stock for a confirmed order. Multi-batch,
// multi-line, single-tx: all reservations commit together or not at all, so a
// partially-stocked order never half-reserves.
//
// arch-test:idempotency-via-reservation-ledger-precheck — CountReservationsForOrder
// short-circuits a redelivery; the messaging Idempotency middleware is the
// primary dedup, this is defence-in-depth.
type ReserveOrderStockHandler struct {
	uow           pg.UnitOfWork
	batches       batch.Repository
	movements     stockmovement.Repository
	ledger        ReservationLedger
	enqueuer      OutboxEnqueuer
	now           func() time.Time
	newMovementID func() stockmovement.ID
}

// NewReserveOrderStockHandler wires the handler. newMovementID is required
// (TestArch_HandlersInjectIDFactory); now nil → time.Now.
func NewReserveOrderStockHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository, ledger ReservationLedger, enqueuer OutboxEnqueuer, now func() time.Time, newMovementID func() stockmovement.ID) ReserveOrderStockHandler {
	if newMovementID == nil {
		panic("command: NewReserveOrderStockHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return ReserveOrderStockHandler{
		uow: uow, batches: batches, movements: movements, ledger: ledger,
		enqueuer: enqueuer, now: now, newMovementID: newMovementID,
	}
}

// allocation is one planned reservation: take Quantity from BatchID (of Product).
type allocation struct {
	productID product.ID
	batchID   batch.ID
	quantity  int64
}

// Handle reserves stock for the order. See [ReserveOrderStockResult].
func (h ReserveOrderStockHandler) Handle(ctx context.Context, cmd ReserveOrderStockCommand) (ReserveOrderStockResult, error) {
	if cmd.TenantID.IsZero() {
		return ReserveOrderStockResult{}, fmt.Errorf("%w: tenant id required", batch.ErrInvalid)
	}
	if cmd.OrderID == "" {
		return ReserveOrderStockResult{}, fmt.Errorf("%w: order id required", batch.ErrInvalid)
	}
	if len(cmd.Lines) == 0 {
		return ReserveOrderStockResult{}, fmt.Errorf("%w: at least one line required", batch.ErrInvalid)
	}

	// Idempotency: a prior delivery already reserved this order → no-op.
	count, err := h.ledger.CountReservationsForOrder(ctx, cmd.TenantID, cmd.OrderID)
	if err != nil {
		return ReserveOrderStockResult{}, fmt.Errorf("reserve order stock: idempotency probe: %w", err)
	}
	if count > 0 {
		return ReserveOrderStockResult{AlreadyReserved: true}, nil
	}

	now := h.now()
	plan, shortfalls, err := h.plan(ctx, cmd, now)
	if err != nil {
		return ReserveOrderStockResult{}, err
	}
	if len(shortfalls) > 0 {
		if eerr := h.emitShortfall(ctx, cmd, shortfalls, now); eerr != nil {
			return ReserveOrderStockResult{}, eerr
		}
		return ReserveOrderStockResult{Shortfalls: shortfalls}, nil
	}

	if err := h.apply(ctx, cmd, plan, now); err != nil {
		return ReserveOrderStockResult{}, err
	}
	return ReserveOrderStockResult{Reserved: true}, nil
}

// plan computes the FEFO allocation for every line. Reads (no lock) outside the
// reservation tx; the in-tx ApplyMovement guard re-validates against locked
// state, so a TOCTOU race rolls back and redelivery re-plans. Returns either a
// full plan (shortfalls empty) or the shortfalls (plan ignored).
func (h ReserveOrderStockHandler) plan(ctx context.Context, cmd ReserveOrderStockCommand, now time.Time) ([]allocation, []integrationevents.ReservationShortfall, error) {
	var plan []allocation
	var shortfalls []integrationevents.ReservationShortfall
	for _, line := range cmd.Lines {
		if line.Quantity <= 0 {
			return nil, nil, fmt.Errorf("%w: line quantity must be > 0 (product %s)", batch.ErrInvalid, line.ProductID)
		}
		fefo, err := h.batches.ListFefoForProduct(ctx, cmd.TenantID, line.ProductID, now)
		if err != nil {
			return nil, nil, fmt.Errorf("reserve order stock: fefo %s: %w", line.ProductID, err)
		}
		remaining := line.Quantity
		var available int64
		for _, b := range fefo {
			avail := b.AvailableQuantity()
			available += avail
			if remaining == 0 || avail <= 0 {
				continue
			}
			take := min(remaining, avail)
			plan = append(plan, allocation{productID: line.ProductID, batchID: b.ID(), quantity: take})
			remaining -= take
		}
		if remaining > 0 {
			pid, perr := uuid.Parse(line.ProductID.String())
			if perr != nil {
				return nil, nil, fmt.Errorf("reserve order stock: parse product id %s: %w", line.ProductID, perr)
			}
			shortfalls = append(shortfalls, integrationevents.ReservationShortfall{
				ProductID:         pid,
				RequestedQuantity: line.Quantity,
				AvailableQuantity: available,
			})
		}
	}
	if len(shortfalls) > 0 {
		return nil, shortfalls, nil
	}
	return plan, nil, nil
}

// apply reserves every planned allocation in ONE tenant tx — all-or-nothing.
func (h ReserveOrderStockHandler) apply(ctx context.Context, cmd ReserveOrderStockCommand, plan []allocation, now time.Time) error {
	ref := cmd.OrderID
	reason := truncateReason("order reservation: " + cmd.OrderID)
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		for _, a := range plan {
			var loaded *batch.Batch
			updErr := h.batches.UpdateByID(ctx, cmd.TenantID, a.batchID, func(b *batch.Batch) (bool, error) {
				if merr := b.ApplyMovement(batch.MovementReservation, a.quantity, now); merr != nil {
					return false, merr
				}
				loaded = b
				return true, nil
			})
			if updErr != nil {
				return fmt.Errorf("reserve order stock: reserve batch %s: %w", a.batchID, updErr)
			}
			m, merr := stockmovement.New(h.newMovementID(), stockmovement.Spec{
				BatchID:             a.batchID,
				ProductID:           a.productID,
				TenantID:            cmd.TenantID,
				Type:                batch.MovementReservation,
				Quantity:            a.quantity,
				QuantityOnHandAfter: loaded.QuantityOnHand(),
				Reason:              reason,
				ActorMembershipID:   cmd.ActorMembershipID,
				SourceReference:     &ref,
			}, now)
			if merr != nil {
				return fmt.Errorf("reserve order stock: construct movement: %w", merr)
			}
			if aerr := h.movements.Add(ctx, m); aerr != nil {
				return fmt.Errorf("reserve order stock: persist movement: %w", aerr)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// emitShortfall publishes StockReservationFailedV1 to the outbox (nothing was
// reserved). A tiny UoW tx carries the outbox write.
func (h ReserveOrderStockHandler) emitShortfall(ctx context.Context, cmd ReserveOrderStockCommand, shortfalls []integrationevents.ReservationShortfall, now time.Time) error {
	orderID, err := uuid.Parse(cmd.OrderID)
	if err != nil {
		return fmt.Errorf("reserve order stock: parse order id %q: %w", cmd.OrderID, err)
	}
	tenantID, err := uuid.Parse(cmd.TenantID.String())
	if err != nil {
		return fmt.Errorf("reserve order stock: parse tenant id: %w", err)
	}
	evt := integrationevents.StockReservationFailedV1{
		OrderID:       orderID,
		TenantIDClaim: tenantID,
		Shortfalls:    shortfalls,
		FailedAtUTC:   now.UTC(),
	}
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		return h.enqueuer.EnqueueInTx(ctx, evt)
	})
}

// truncateReason caps a reason at stockmovement.ReasonMaxLength.
func truncateReason(s string) string {
	if len(s) > stockmovement.ReasonMaxLength {
		return s[:stockmovement.ReasonMaxLength]
	}
	return s
}
