package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// LogStockMovementCommand is the request payload for the POST
// /v1/inventory/batches/{id}/movements endpoint.
//
// Quantity is the MAGNITUDE — caller passes a positive number for
// Inbound / Outbound / Reservation / Release, and any non-zero for
// Adjustment. The handler converts to the SIGNED ledger convention
// before persisting (Outbound becomes negative).
type LogStockMovementCommand struct {
	BatchID           batch.ID
	ActorMembershipID membership.ID
	Type              batch.MovementType
	Quantity          int64
	Reason            string
	SourceReference   *string
}

// LogStockMovementResult carries the new MovementID + the batch's
// post-mutation quantity_on_hand (handy for SPA optimistic-update
// reconciliation).
type LogStockMovementResult struct {
	MovementID          stockmovement.ID
	QuantityOnHandAfter int64
}

// LogStockMovementHandler — the slice's central orchestration.
//
// Multi-aggregate single-tx write per ADR 0008 + Vernon ch.10:
//   1. Open UoW tx (TxScopeTenant — RLS-bound to caller's tenant)
//   2. UpdateByID the Batch (loads → ApplyMovement → persists with
//      optimistic-concurrency check → drains AddedEvent — but Batch
//      doesn't emit a new event on ApplyMovement; that's the
//      Movement's job)
//   3. Add a new StockMovement row inside the SAME tx (joins via
//      pg.TxFromContext) — emits LoggedEvent → outbox.
//
// Optimistic-concurrency retry: on batch.ErrConcurrencyConflict we
// retry up to maxConcurrencyRetries times. Each retry re-reads the
// batch + re-applies the movement. The handler's ApplyMovement guards
// (insufficient stock, expired) re-evaluate against the fresh state —
// a movement that was valid against the stale read may now be
// rejected, which surfaces as the more-specific error (e.g.
// ErrInsufficientStock) without further retry.
//
// Idempotency: per ADR 0031, the X-Command-Id middleware catches
// duplicate requests at the HTTP boundary; this handler does NOT
// re-check.
type LogStockMovementHandler struct {
	uow       pg.UnitOfWork
	batches   batch.Repository
	movements stockmovement.Repository
}

// NewLogStockMovementHandler wires the handler.
func NewLogStockMovementHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository) LogStockMovementHandler {
	return LogStockMovementHandler{uow: uow, batches: batches, movements: movements}
}

// maxConcurrencyRetries caps the optimistic-concurrency retry loop.
// 3 attempts handles realistic burst contention (concurrent Order
// fulfilment racing the same batch) without burning resources on a
// pathological hot-row scenario — that signals "you need pessimistic
// locking or a queue" which is outside slice 1.
const maxConcurrencyRetries = 3

// Handle persists a stock movement against the supplied batch.
func (h LogStockMovementHandler) Handle(ctx context.Context, cmd LogStockMovementCommand) (LogStockMovementResult, error) {
	if !cmd.Type.IsValid() {
		return LogStockMovementResult{}, fmt.Errorf("%w: unknown movement type %q", batch.ErrInvalid, cmd.Type)
	}
	if cmd.Quantity == 0 {
		return LogStockMovementResult{}, fmt.Errorf("%w: quantity must be non-zero", batch.ErrInvalid)
	}
	// Magnitude must be positive — caller-supplied negative quantity is
	// rejected; the handler controls the signed convention.
	magnitude := cmd.Quantity
	if magnitude < 0 {
		return LogStockMovementResult{}, fmt.Errorf("%w: quantity must be a positive magnitude (got %d)", batch.ErrInvalid, magnitude)
	}

	var result LogStockMovementResult
	var lastErr error
	for range maxConcurrencyRetries {
		result, lastErr = h.attemptOnce(ctx, cmd, magnitude)
		if lastErr == nil {
			return result, nil
		}
		if !errors.Is(lastErr, batch.ErrConcurrencyConflict) {
			return LogStockMovementResult{}, lastErr
		}
		// loop and retry — fresh read picks up the racer's update
	}
	return LogStockMovementResult{}, fmt.Errorf("log stock movement: gave up after %d retries: %w",
		maxConcurrencyRetries, lastErr)
}

// attemptOnce performs ONE try of the multi-aggregate single-tx write.
// On ErrConcurrencyConflict the caller's loop retries; on any other
// error we abort.
func (h LogStockMovementHandler) attemptOnce(ctx context.Context, cmd LogStockMovementCommand, magnitude int64) (LogStockMovementResult, error) {
	var result LogStockMovementResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var loaded *batch.Batch
		updateErr := h.batches.UpdateByID(ctx, cmd.BatchID, func(b *batch.Batch) (bool, error) {
			if err := b.ApplyMovement(cmd.Type, magnitude); err != nil {
				return false, err
			}
			loaded = b
			// Persist always — ApplyMovement bumps version on every
			// successful call (including non-mutating Reservation /
			// Release) so the row write must follow.
			return true, nil
		})
		if updateErr != nil {
			return updateErr
		}
		// loaded is the post-ApplyMovement state. Construct the
		// StockMovement with the SIGNED quantity + new on-hand snapshot.
		signed := signedQuantityForType(cmd.Type, magnitude)
		m, err := stockmovement.New(stockmovement.ID(ids.NewV7().String()), stockmovement.Spec{
			BatchID:             loaded.ID(),
			ProductID:           loaded.ProductID(),
			TenantID:            loaded.TenantID(),
			Type:                cmd.Type,
			Quantity:            signed,
			QuantityOnHandAfter: loaded.QuantityOnHand(),
			Reason:              cmd.Reason,
			ActorMembershipID:   cmd.ActorMembershipID,
			SourceReference:     cmd.SourceReference,
		})
		if err != nil {
			return fmt.Errorf("log stock movement: construct: %w", err)
		}
		if err := h.movements.Add(ctx, m); err != nil {
			return fmt.Errorf("log stock movement: persist movement: %w", err)
		}
		result = LogStockMovementResult{
			MovementID:          m.ID(),
			QuantityOnHandAfter: loaded.QuantityOnHand(),
		}
		return nil
	})
	if err != nil {
		return LogStockMovementResult{}, err
	}
	return result, nil
}

// signedQuantityForType applies the ledger SIGN convention per
// StockMovement's documented contract:
//   - Inbound / Reservation / Release / Adjustment: positive magnitude as-is.
//   - Outbound: negate (caller passed magnitude>0; ledger stores negative).
//
// Adjustment direction (count-up vs shrinkage) is NOT disambiguated at
// the wire in Slice 1 — both ride positive magnitude. When product
// asks for the distinction, add a `direction` enum field to
// LogMovementRequest + branch here per ADR 0061 §"Deferred".
func signedQuantityForType(mt batch.MovementType, magnitude int64) int64 {
	if mt == batch.MovementOutbound {
		return -magnitude
	}
	return magnitude
}
