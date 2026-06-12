package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// HandlerCancelCompensation is the CI-stable handler name. DO NOT rename.
const HandlerCancelCompensation = "orders.subscribers.CancelCompensation"

// arch-test:idempotency-via-natural-key-precheck — the cancellation note is
// unique per invoice (uq_orders_credit_notes_cancellation); the compensation
// handler treats ErrCancellationAlreadyExists as success, so replays ack.

// CancelCompensationSubscriber is the in-module financial compensation for
// `orders.order_cancelled.v1` (ADR 0063 §4): a cancel at/after invoicing mints
// a cancellation note reversing the invoice. Pre-invoice cancels need no
// reversal; post-delivery cancels are operator-driven returns (partial
// amounts) and are logged, never auto-minted.
type CancelCompensationSubscriber struct {
	compensate command.CompensateOrderCancellationHandler
	log        *slog.Logger
}

// NewCancelCompensationSubscriber wires the subscriber. log is required.
func NewCancelCompensationSubscriber(compensate command.CompensateOrderCancellationHandler, log *slog.Logger) *CancelCompensationSubscriber {
	if log == nil {
		panic("subscribers: NewCancelCompensationSubscriber log required")
	}
	return &CancelCompensationSubscriber{compensate: compensate, log: log}
}

// Handle is the typed cqrs handler for `orders.order_cancelled.v1`.
func (h *CancelCompensationSubscriber) Handle(ctx context.Context, evt *integrationevents.OrderCancelledV1) error {
	prior := order.State(evt.PriorState)
	if prior == order.StateDelivered {
		h.log.InfoContext(ctx, "orders: post-delivery cancel — credit note is an operator decision, no auto-reversal",
			"order_id", evt.OrderID)
		return nil
	}
	res, err := h.compensate.Handle(ctx, command.CompensateOrderCancellationCommand{
		TenantID:           tenant.ID(evt.TenantID),
		OrderID:            order.ID(evt.OrderID),
		PriorState:         prior,
		Reason:             evt.Reason,
		IssuedByMembership: membership.ID(evt.CancelledByMembership),
	})
	if err != nil {
		return fmt.Errorf("orders cancel-compensation: %w", err)
	}
	if res.Minted {
		h.log.InfoContext(ctx, "orders: cancellation note minted",
			"order_id", evt.OrderID, "credit_note_id", res.CreditNoteID.String(), "prior_state", evt.PriorState)
	}
	return nil
}
