package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// HandlerOrderCancelled is the CI-stable handler name. DO NOT rename.
const HandlerOrderCancelled = "dispatch.subscribers.OrderCancelled"

// arch-test:idempotency-via-state-machine — MarkFailed is self-idempotent
// (failed → failed is a no-op) and prior-state cancels with no consignment ack
// immediately, so at-least-once replays converge.

// priorStatesWithConsignment are the order states (wire strings from
// orders.order_cancelled.v1 — the event IS the cross-module contract, dispatch
// never imports orders/domain) at which a consignment slot exists or is being
// minted. For these the handler retries until the slot is visible; for earlier
// states no slot will ever exist and the event acks immediately.
var priorStatesWithConsignment = map[string]bool{
	"packed":     true,
	"invoiced":   true,
	"dispatched": true,
}

// OrderCancelledSubscriber is Dispatch's compensation for
// `orders.order_cancelled.v1` (ADR 0063 §4): fail the consignment so the
// warehouse pulls it from the carrier flow. A consignment already DELIVERED is
// a genuine business conflict (goods handed over, order cancelled) — that
// errors → retry → durable DLQ, which is correct: a human must resolve it.
type OrderCancelledSubscriber struct {
	byOrder    query.GetConsignmentNoteByOrderHandler
	markFailed command.MarkFailedHandler
	log        *slog.Logger
}

// NewOrderCancelledSubscriber wires the subscriber. log is required.
func NewOrderCancelledSubscriber(
	byOrder query.GetConsignmentNoteByOrderHandler,
	markFailed command.MarkFailedHandler,
	log *slog.Logger,
) *OrderCancelledSubscriber {
	if log == nil {
		panic("subscribers: NewOrderCancelledSubscriber log required")
	}
	return &OrderCancelledSubscriber{byOrder: byOrder, markFailed: markFailed, log: log}
}

// Handle is the typed cqrs handler for `orders.order_cancelled.v1`.
func (h *OrderCancelledSubscriber) Handle(ctx context.Context, evt *ordersevents.OrderCancelledV1) error {
	if !priorStatesWithConsignment[evt.PriorState] {
		return nil // cancelled before packing — no consignment will ever exist
	}
	tid := tenant.ID(evt.TenantID)
	note, err := h.byOrder.Handle(ctx, query.GetConsignmentNoteByOrderQuery{
		TenantID: tid,
		OrderID:  consignmentnote.OrderID(evt.OrderID),
	})
	if err != nil {
		if errors.Is(err, query.ErrConsignmentNoteNotFound) {
			// PriorState says the slot exists or is in flight (the
			// order_packed ingestor mints it) — retry until visible.
			return fmt.Errorf("dispatch order-cancelled: consignment not yet visible for order %s: %w", evt.OrderID, err)
		}
		return fmt.Errorf("dispatch order-cancelled: load consignment: %w", err)
	}
	if err := h.markFailed.Handle(ctx, command.MarkFailedCommand{
		TenantID:                 tid,
		ConsignmentNoteID:        consignmentnote.ID(note.ID),
		Reason:                   "order cancelled: " + evt.Reason,
		TransitionedByMembership: membership.ID(evt.CancelledByMembership),
	}); err != nil {
		return fmt.Errorf("dispatch order-cancelled: mark failed: %w", err)
	}
	h.log.InfoContext(ctx, "dispatch: consignment failed on order cancellation",
		"order_id", evt.OrderID, "consignment_note_id", note.ID)
	return nil
}
