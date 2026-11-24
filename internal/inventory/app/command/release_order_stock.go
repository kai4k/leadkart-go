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

// ReleaseOrderStockCommand returns an order's outstanding reserved stock to
// available — the cancellation compensation (ADR 0063 §4). State-agnostic: it
// releases whatever the ledger still shows held, so a cancel BEFORE pack
// releases the hold while a cancel AFTER consumption (ledger already zero)
// no-ops cleanly.
type ReleaseOrderStockCommand struct {
	TenantID          tenant.ID
	OrderID           string
	ActorMembershipID membership.ID
	Reason            string
}

// ReleaseOrderStockResult reports whether anything was released and across how
// many batches. Released=false is the idempotent no-op (nothing outstanding).
type ReleaseOrderStockResult struct {
	Released   bool
	BatchCount int
}

// ReleaseOrderStockHandler releases an order's reservation across all holding
// batches in one tx.
//
// arch-test:idempotency-via-outstanding-ledger — a re-delivered cancel sees an
// empty outstanding set (the prior release zeroed it) and no-ops.
type ReleaseOrderStockHandler struct {
	uow           pg.UnitOfWork
	batches       batch.Repository
	movements     stockmovement.Repository
	ledger        ReservationLedger
	now           func() time.Time
	newMovementID func() stockmovement.ID
}

// NewReleaseOrderStockHandler wires the handler. newMovementID required;
// now nil → time.Now.
func NewReleaseOrderStockHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository, ledger ReservationLedger, now func() time.Time, newMovementID func() stockmovement.ID) ReleaseOrderStockHandler {
	if newMovementID == nil {
		panic("command: NewReleaseOrderStockHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return ReleaseOrderStockHandler{
		uow: uow, batches: batches, movements: movements, ledger: ledger,
		now: now, newMovementID: newMovementID,
	}
}

// Handle releases the order's outstanding reservation.
func (h ReleaseOrderStockHandler) Handle(ctx context.Context, cmd ReleaseOrderStockCommand) (ReleaseOrderStockResult, error) {
	if cmd.TenantID.IsZero() {
		return ReleaseOrderStockResult{}, fmt.Errorf("%w: tenant id required", batch.ErrInvalid)
	}
	if cmd.OrderID == "" {
		return ReleaseOrderStockResult{}, fmt.Errorf("%w: order id required", batch.ErrInvalid)
	}
	now := h.now()
	ref := cmd.OrderID
	reason := truncateReason(fmt.Sprintf("order release: %s: %s", cmd.OrderID, cmd.Reason))
	var outstanding []stockmovement.OutstandingReservation
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		// Lock the batches, THEN read the hold: see lockedOrderHold.
		held, herr := lockedOrderHold(ctx, h.batches, h.ledger, cmd.TenantID, cmd.OrderID)
		if herr != nil {
			return fmt.Errorf("release order stock: %w", herr)
		}
		outstanding = held
		for _, o := range outstanding {
			var onHandAfter int64
			updErr := h.batches.UpdateByID(ctx, cmd.TenantID, o.BatchID, func(b *batch.Batch) (bool, error) {
				if merr := b.ApplyMovement(batch.MovementRelease, o.Quantity, now, now); merr != nil {
					return false, merr
				}
				onHandAfter = b.QuantityOnHand()
				return true, nil
			})
			if updErr != nil {
				return fmt.Errorf("release order stock: release batch %s: %w", o.BatchID, updErr)
			}
			m, merr := stockmovement.New(h.newMovementID(), stockmovement.Spec{
				BatchID:             o.BatchID,
				ProductID:           o.ProductID,
				TenantID:            cmd.TenantID,
				Type:                batch.MovementRelease,
				Quantity:            o.Quantity,
				QuantityOnHandAfter: onHandAfter,
				Reason:              reason,
				ActorMembershipID:   cmd.ActorMembershipID,
				SourceReference:     &ref,
			}, now)
			if merr != nil {
				return fmt.Errorf("release order stock: construct movement: %w", merr)
			}
			if aerr := h.movements.Add(ctx, m); aerr != nil {
				return fmt.Errorf("release order stock: persist movement: %w", aerr)
			}
		}
		return nil
	})
	if err != nil {
		return ReleaseOrderStockResult{}, err
	}
	if len(outstanding) == 0 {
		return ReleaseOrderStockResult{}, nil
	}
	return ReleaseOrderStockResult{Released: true, BatchCount: len(outstanding)}, nil
}
