// Package command holds Orders CQRS command handlers.
//
// TDL Wild Workouts shape: each handler is a concrete struct with one Handle
// method, exposed as a field on the [app.Application] facade. Boundary
// discipline (ADR 0047): handlers depend only on domain repository interfaces
// + cross-cutting infra interfaces (pg.UnitOfWork, OutboxEnqueuer,
// invoicenumber.Allocator) — never pgx or a concrete adapter.
package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// OutboxEnqueuer emits integration events the aggregate's own domain event
// cannot carry: OrderConfirmedV1 (line snapshot for Inventory) + OrderPackedV1
// (carrier logistics for Dispatch) hold derived data, so the confirming /
// packing handlers build them and enqueue here; the adapter writes to the
// shared outbox inside the active UoW tx (ADR 0063 §4).
type OutboxEnqueuer interface {
	// EnqueueInTx writes events to the outbox inside the active UoW tx (from
	// ctx via pg.TxFromContext). Errors if no tx is active — call only from
	// within a UoW.WithinTx closure.
	EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error
}
