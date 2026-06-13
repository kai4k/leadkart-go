package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// ReservationLedger reads the stock-movement ledger to reconstruct an order's
// outstanding reserved quantity per batch — the true-hold release/consume
// source of truth (ADR 0063 §4), since the Orders events carry no line
// snapshot. Implemented by the StockMovementRepository (consumer-owned
// interface per TDL; the adapter satisfies it structurally).
type ReservationLedger interface {
	// CountReservationsForOrder returns how many reservation rows exist for
	// the order (source_reference). The reserve handler no-ops when > 0
	// (idempotency under at-least-once redelivery).
	CountReservationsForOrder(ctx context.Context, tenantID tenant.ID, orderRef string) (int64, error)

	// ListOutstandingReservationsForOrder returns the net still-held reserved
	// quantity per batch for the order. Empty → nothing to release/consume.
	ListOutstandingReservationsForOrder(ctx context.Context, tenantID tenant.ID, orderRef string) ([]stockmovement.OutstandingReservation, error)
}

// OutboxEnqueuer emits integration events no aggregate carries — here
// StockReservationFailedV1 (the shortfall snapshot). The adapter writes to the
// shared outbox inside the active UoW tx (ADR 0064/0067).
type OutboxEnqueuer interface {
	// EnqueueInTx writes events to the outbox inside the active UoW tx (from
	// ctx via pg.TxFromContext). Errors if no tx is active.
	EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error
}
