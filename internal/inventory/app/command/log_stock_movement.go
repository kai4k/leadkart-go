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
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// LogStockMovementCommand is the payload for POST
// /v1/inventory/batches/{id}/movements.
//
// Quantity is the magnitude (positive); the handler applies the signed ledger
// convention before persisting (Outbound becomes negative).
//
// TenantID is the caller's tenant scope; per ADR 0062 (TDL canon) it flows
// through an explicit field, never via context smuggling.
type LogStockMovementCommand struct {
	TenantID          tenant.ID
	BatchID           batch.ID
	ActorMembershipID membership.ID
	Type              batch.MovementType
	Quantity          int64
	Reason            string
	SourceReference   *string
}

// LogStockMovementResult carries the new MovementID and the batch's
// post-mutation quantity_on_hand (for SPA optimistic-update reconciliation).
type LogStockMovementResult struct {
	MovementID          stockmovement.ID
	QuantityOnHandAfter int64
}

// LogStockMovementHandler is the slice's central orchestration: a
// multi-aggregate single-tx write (ADR 0008 + Vernon ch.10) that updates the
// Batch and adds a StockMovement in the same UoW tx, emitting LoggedEvent.
//
// No retry loop. Batch UpdateByID takes a pessimistic row lock
// (SELECT ... FOR UPDATE), so concurrent writers serialise at the DB and
// batch.ErrConcurrencyConflict is unreachable; ApplyMovement guards always run
// against the latest committed state. Per Postgres §13.3.2, the Stripe ledger
// pattern, and DDIA Ch.7, pessimistic locks beat optimistic retries for
// hot-row counters where contention is the norm.
//
// Idempotency: per ADR 0031 the X-Command-Id middleware dedupes at the HTTP
// boundary; this handler does not re-check.
type LogStockMovementHandler struct {
	uow           pg.UnitOfWork
	batches       batch.Repository
	movements     stockmovement.Repository
	now           func() time.Time
	newMovementID func() stockmovement.ID
}

// NewLogStockMovementHandler wires the handler. now is the injected clock
// (nil → time.Now). newMovementID is the StockMovement-ID factory required by
// TestArch_HandlersInjectIDFactory; tests inject a deterministic one.
func NewLogStockMovementHandler(uow pg.UnitOfWork, batches batch.Repository, movements stockmovement.Repository, now func() time.Time, newMovementID func() stockmovement.ID) LogStockMovementHandler {
	if newMovementID == nil {
		panic("command: NewLogStockMovementHandler newMovementID required")
	}
	if now == nil {
		now = time.Now
	}
	return LogStockMovementHandler{
		uow: uow, batches: batches, movements: movements,
		now: now, newMovementID: newMovementID,
	}
}

// Handle persists a stock movement against the supplied batch.
func (h LogStockMovementHandler) Handle(ctx context.Context, cmd LogStockMovementCommand) (LogStockMovementResult, error) {
	if cmd.TenantID.IsZero() {
		return LogStockMovementResult{}, errors.New("log_stock_movement: tenant id required")
	}
	if !cmd.Type.IsValid() {
		return LogStockMovementResult{}, fmt.Errorf("%w: unknown movement type %q", batch.ErrInvalid, cmd.Type)
	}
	if cmd.Quantity == 0 {
		return LogStockMovementResult{}, fmt.Errorf("%w: quantity must be non-zero", batch.ErrInvalid)
	}
	// Magnitude must be positive; the handler owns the sign convention.
	magnitude := cmd.Quantity
	if magnitude < 0 {
		return LogStockMovementResult{}, fmt.Errorf("%w: quantity must be a positive magnitude (got %d)", batch.ErrInvalid, magnitude)
	}
	return h.persist(ctx, cmd, magnitude, h.now())
}

// persist runs the multi-aggregate single-tx write. Batch UpdateByID acquires
// SELECT FOR UPDATE; the concurrency story lives in [BatchRepository.updateOnTx].
//
// now is captured once in Handle so Batch.ApplyMovement's updatedAt and
// StockMovement.occurredAt match exactly in audit/ledger queries.
func (h LogStockMovementHandler) persist(ctx context.Context, cmd LogStockMovementCommand, magnitude int64, now time.Time) (LogStockMovementResult, error) {
	var result LogStockMovementResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var loaded *batch.Batch
		updateErr := h.batches.UpdateByID(ctx, cmd.TenantID, cmd.BatchID, func(b *batch.Batch) (bool, error) {
			if err := b.ApplyMovement(cmd.Type, magnitude, now); err != nil {
				return false, err
			}
			loaded = b
			// Persist always: ApplyMovement bumps version on every
			// successful call, so the row write must follow.
			return true, nil
		})
		if updateErr != nil {
			return updateErr
		}
		// Construct the StockMovement from the post-ApplyMovement state
		// with the signed quantity and new on-hand snapshot.
		signed := signedQuantityForType(cmd.Type, magnitude)
		m, err := stockmovement.New(h.newMovementID(), stockmovement.Spec{
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

// signedQuantityForType applies the ledger sign convention: Outbound negates,
// everything else keeps the positive magnitude.
//
// Adjustment direction (count-up vs shrinkage) is not disambiguated in Slice 1;
// add a direction enum and branch here when product asks (ADR 0061 §"Deferred").
func signedQuantityForType(mt batch.MovementType, magnitude int64) int64 {
	if mt == batch.MovementOutbound {
		return -magnitude
	}
	return magnitude
}
