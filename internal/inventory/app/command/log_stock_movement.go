package command

import (
	"context"
	"fmt"
	"time"

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
//   2. UpdateByID the Batch — the adapter acquires a pessimistic row
//      lock (SELECT ... FOR UPDATE) before the load, so concurrent
//      writers serialize at the DB layer; the in-memory ApplyMovement
//      then runs against the latest committed state, and the row is
//      persisted with version-bump as a defense-in-depth signal.
//   3. Add a new StockMovement row inside the SAME tx (joins via
//      pg.TxFromContext) — emits LoggedEvent → outbox.
//
// No retry loop: by construction the pessimistic lock makes
// batch.ErrConcurrencyConflict unreachable in production. Concurrent
// LogStockMovement calls against the same batch block at the row lock
// (typical wait < 1ms in the LogStockMovement hot path) and run in a
// serial-order schedule. ApplyMovement guards (insufficient stock,
// expired) ALWAYS evaluate against the latest committed state —
// there is no stale-snapshot path.
//
// Canon: Postgres §13.3.2 explicit locking + Stripe ledger pattern +
// DDIA Ch.7 — pessimistic locks beat optimistic retries for hot-row
// counters where contention is the norm, not the exception.
//
// Idempotency: per ADR 0031, the X-Command-Id middleware catches
// duplicate requests at the HTTP boundary; this handler does NOT
// re-check.
type LogStockMovementHandler struct {
	uow       pg.UnitOfWork
	batches   batch.Repository
	movements stockmovement.Repository
	now       func() time.Time
}

// NewLogStockMovementHandler wires the handler. `now` is the explicit
// time source — composition root passes `time.Now`; tests inject a
// fixed-time closure for deterministic assertions. Nil → time.Now.
func NewLogStockMovementHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository, now func() time.Time) LogStockMovementHandler {
	if now == nil {
		now = time.Now
	}
	return LogStockMovementHandler{uow: uow, batches: batches, movements: movements, now: now}
}

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
	return h.persist(ctx, cmd, magnitude, h.now())
}

// persist runs the multi-aggregate single-tx write inside one UoW.
// The batch repository's UpdateByID acquires SELECT FOR UPDATE — the
// concurrency story lives in [BatchRepository.updateOnTx], NOT here.
//
// `now` is the shared instant captured once at the top of Handle so the
// Batch.ApplyMovement updatedAt + StockMovement.occurredAt line up
// byte-for-byte in audit / ledger queries.
func (h LogStockMovementHandler) persist(ctx context.Context, cmd LogStockMovementCommand, magnitude int64, now time.Time) (LogStockMovementResult, error) {
	var result LogStockMovementResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var loaded *batch.Batch
		updateErr := h.batches.UpdateByID(ctx, cmd.BatchID, func(b *batch.Batch) (bool, error) {
			if err := b.ApplyMovement(cmd.Type, magnitude, now); err != nil {
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
		}, now)
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
