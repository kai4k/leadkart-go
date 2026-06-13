package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// ConsumeOrderStockCommand realises an order's reservation as a physical
// shipment when the order is packed (ADR 0063 §4): for every holding batch it
// releases the hold AND ships the units out (Release + Outbound in one tx), so
// reserved−q and on-hand−q. After consume the order's outstanding hold is zero.
type ConsumeOrderStockCommand struct {
	TenantID          tenant.ID
	OrderID           string
	ActorMembershipID membership.ID
}

// ConsumeOrderStockResult reports whether stock was consumed and across how
// many batches. Consumed=false is the idempotent no-op (nothing outstanding —
// already consumed, or cancelled before pack).
type ConsumeOrderStockResult struct {
	Consumed   bool
	BatchCount int
}

// ConsumeOrderStockHandler consumes an order's reservation across all holding
// batches in one tx.
//
// arch-test:idempotency-via-outstanding-ledger — a re-delivered pack sees an
// empty outstanding set (the prior consume zeroed it) and no-ops.
type ConsumeOrderStockHandler struct {
	uow           pg.UnitOfWork
	batches       batch.Repository
	movements     stockmovement.Repository
	ledger        ReservationLedger
	now           func() time.Time
	newMovementID func() stockmovement.ID
}

// NewConsumeOrderStockHandler wires the handler. newMovementID required;
// now nil → time.Now.
func NewConsumeOrderStockHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository, ledger ReservationLedger, now func() time.Time, newMovementID func() stockmovement.ID) ConsumeOrderStockHandler {
	if newMovementID == nil {
		panic("command: NewConsumeOrderStockHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return ConsumeOrderStockHandler{
		uow: uow, batches: batches, movements: movements, ledger: ledger,
		now: now, newMovementID: newMovementID,
	}
}

// Handle consumes the order's outstanding reservation as a shipment.
func (h ConsumeOrderStockHandler) Handle(ctx context.Context, cmd ConsumeOrderStockCommand) (ConsumeOrderStockResult, error) {
	if cmd.TenantID.IsZero() {
		return ConsumeOrderStockResult{}, fmt.Errorf("%w: tenant id required", batch.ErrInvalid)
	}
	if cmd.OrderID == "" {
		return ConsumeOrderStockResult{}, fmt.Errorf("%w: order id required", batch.ErrInvalid)
	}
	outstanding, err := h.ledger.ListOutstandingReservationsForOrder(ctx, cmd.TenantID, cmd.OrderID)
	if err != nil {
		return ConsumeOrderStockResult{}, fmt.Errorf("consume order stock: load outstanding: %w", err)
	}
	if len(outstanding) == 0 {
		return ConsumeOrderStockResult{}, nil
	}

	now := h.now()
	ref := cmd.OrderID
	releaseReason := truncateReason("order consume (release hold): " + cmd.OrderID)
	shipReason := truncateReason("order consume (shipped): " + cmd.OrderID)
	err = h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		for _, o := range outstanding {
			var releaseOnHand, shipOnHand int64
			updErr := h.batches.UpdateByID(ctx, cmd.TenantID, o.BatchID, func(b *batch.Batch) (bool, error) {
				if merr := b.ApplyMovement(batch.MovementRelease, o.Quantity, now); merr != nil {
					return false, merr
				}
				releaseOnHand = b.QuantityOnHand() // unchanged by release
				if merr := b.ApplyMovement(batch.MovementOutbound, o.Quantity, now); merr != nil {
					return false, merr
				}
				shipOnHand = b.QuantityOnHand() // reduced by outbound
				return true, nil
			})
			if updErr != nil {
				return fmt.Errorf("consume order stock: consume batch %s: %w", o.BatchID, updErr)
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
				return fmt.Errorf("consume order stock: construct release: %w", merr)
			}
			if aerr := h.movements.Add(ctx, rel); aerr != nil {
				return fmt.Errorf("consume order stock: persist release: %w", aerr)
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
				return fmt.Errorf("consume order stock: construct outbound: %w", merr)
			}
			if aerr := h.movements.Add(ctx, out); aerr != nil {
				return fmt.Errorf("consume order stock: persist outbound: %w", aerr)
			}
		}
		return nil
	})
	if err != nil {
		return ConsumeOrderStockResult{}, err
	}
	return ConsumeOrderStockResult{Consumed: true, BatchCount: len(outstanding)}, nil
}
