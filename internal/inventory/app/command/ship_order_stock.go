package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// ErrOrderStockNotHeld is returned when an order holds no stock at the moment goods leave
// for it: goods never leave without a deduction, so the shipment is refused rather than
// recorded as a no-op.
var ErrOrderStockNotHeld = errors.New("inventory: the order holds no stock to ship")

// ShipOrderStockCommand realises an order's hold as a physical shipment when the goods leave
// (ADR 0072): for every holding batch it releases the hold AND ships the units out, so
// reserved-q and on-hand-q, with both movement rows. After the shipment the order's
// outstanding hold is zero.
type ShipOrderStockCommand struct {
	TenantID          tenant.ID
	OrderID           string
	ActorMembershipID membership.ID
}

// ShipOrderStockResult reports across how many batches the order shipped.
type ShipOrderStockResult struct {
	BatchCount int
}

// ShipOrderStockHandler ships an order's hold across all holding batches. It is ONE STEP of
// Dispatch's dispatch transaction and owns no transaction: every repository it calls joins the
// transaction in ctx, and the composition-root bridge refuses a ctx that carries none, so the
// stock cannot move without the order and the consignment note moving with it.
//
// It reads the hold with the shared lock-then-read routine ([lockedOrderHold]), so two
// concurrent runs drain a hold once and never touch another order's hold.
//
// arch-test:idempotency-via-outstanding-ledger — a second shipment of the same order sees an
// empty hold (the first zeroed it) and is refused with [ErrOrderStockNotHeld]; the dispatch
// transaction's note lock makes that case unreachable for one note.
type ShipOrderStockHandler struct {
	batches       batch.Repository
	movements     stockmovement.Repository
	ledger        ReservationLedger
	window        sellability.Resolver
	now           func() time.Time
	newMovementID func() stockmovement.ID
}

// NewShipOrderStockHandler wires the handler. newMovementID required; now nil → time.Now.
func NewShipOrderStockHandler(batches batch.Repository, movements stockmovement.Repository, ledger ReservationLedger, window sellability.Resolver, now func() time.Time, newMovementID func() stockmovement.ID) ShipOrderStockHandler {
	if newMovementID == nil {
		panic("command: NewShipOrderStockHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return ShipOrderStockHandler{
		batches: batches, movements: movements, ledger: ledger, window: window,
		now: now, newMovementID: newMovementID,
	}
}

// Handle ships the order's outstanding hold. No hold: [ErrOrderStockNotHeld]. A held batch
// past its printed expiry on the company business day: [batch.ErrExpired] (the outbound is
// refused). Either way the caller's transaction rolls back.
func (h ShipOrderStockHandler) Handle(ctx context.Context, cmd ShipOrderStockCommand) (ShipOrderStockResult, error) {
	if cmd.TenantID.IsZero() {
		return ShipOrderStockResult{}, fmt.Errorf("%w: tenant id required", batch.ErrInvalid)
	}
	if cmd.OrderID == "" {
		return ShipOrderStockResult{}, fmt.Errorf("%w: order id required", batch.ErrInvalid)
	}
	// Lock the batches, THEN read the hold: see lockedOrderHold.
	outstanding, err := lockedOrderHold(ctx, h.batches, h.ledger, cmd.TenantID, cmd.OrderID)
	if err != nil {
		return ShipOrderStockResult{}, fmt.Errorf("ship order stock: %w", err)
	}
	if len(outstanding) == 0 {
		return ShipOrderStockResult{}, ErrOrderStockNotHeld
	}

	now := h.now()
	// The shipment is judged on the company business day: units reserved before the
	// printed expiry cannot ship after it (the outbound is refused, the tx rolls back).
	w, err := h.window.Resolve(ctx, cmd.TenantID)
	if err != nil {
		return ShipOrderStockResult{}, fmt.Errorf("ship order stock: %w", err)
	}
	ref := cmd.OrderID
	releaseReason := truncateReason("order shipped (release hold): " + cmd.OrderID)
	shipReason := truncateReason("order shipped: " + cmd.OrderID)
	for _, o := range outstanding {
		var releaseOnHand, shipOnHand int64
		updErr := h.batches.UpdateByID(ctx, cmd.TenantID, o.BatchID, func(b *batch.Batch) (bool, error) {
			if merr := b.ApplyMovement(batch.MovementRelease, o.Quantity, now, w.Today); merr != nil {
				return false, merr
			}
			releaseOnHand = b.QuantityOnHand() // unchanged by release
			if merr := b.ApplyMovement(batch.MovementOutbound, o.Quantity, now, w.Today); merr != nil {
				return false, merr
			}
			shipOnHand = b.QuantityOnHand() // reduced by outbound
			return true, nil
		})
		if updErr != nil {
			return ShipOrderStockResult{}, fmt.Errorf("ship order stock: ship batch %s: %w", o.BatchID, updErr)
		}
		// Two ledger rows: the released hold, then the physical outbound.
		rel, merr := stockmovement.New(h.newMovementID(), stockmovement.Spec{
			BatchID:             o.BatchID,
			ProductID:           o.ProductID,
			TenantID:            cmd.TenantID,
			Type:                batch.MovementRelease,
			Quantity:            o.Quantity,
			QuantityOnHandAfter: releaseOnHand,
			Reason:              releaseReason,
			ActorMembershipID:   cmd.ActorMembershipID,
			SourceReference:     &ref,
		}, now)
		if merr != nil {
			return ShipOrderStockResult{}, fmt.Errorf("ship order stock: construct release: %w", merr)
		}
		if aerr := h.movements.Add(ctx, rel); aerr != nil {
			return ShipOrderStockResult{}, fmt.Errorf("ship order stock: persist release: %w", aerr)
		}
		out, merr := stockmovement.New(h.newMovementID(), stockmovement.Spec{
			BatchID:             o.BatchID,
			ProductID:           o.ProductID,
			TenantID:            cmd.TenantID,
			Type:                batch.MovementOutbound,
			Quantity:            signedQuantityForType(batch.MovementOutbound, o.Quantity),
			QuantityOnHandAfter: shipOnHand,
			Reason:              shipReason,
			ActorMembershipID:   cmd.ActorMembershipID,
			SourceReference:     &ref,
		}, now)
		if merr != nil {
			return ShipOrderStockResult{}, fmt.Errorf("ship order stock: construct outbound: %w", merr)
		}
		if aerr := h.movements.Add(ctx, out); aerr != nil {
			return ShipOrderStockResult{}, fmt.Errorf("ship order stock: persist outbound: %w", aerr)
		}
	}
	return ShipOrderStockResult{BatchCount: len(outstanding)}, nil
}
