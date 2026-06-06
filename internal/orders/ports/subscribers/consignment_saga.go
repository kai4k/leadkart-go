package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	dispatchevents "github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// HandlerConsignmentCreated + HandlerConsignmentDelivered are the CI-stable
// handler names for the cross-module fulfillment-saga steps. DO NOT rename.
const (
	HandlerConsignmentCreated   = "orders.subscribers.ConsignmentCreated"
	HandlerConsignmentDelivered = "orders.subscribers.ConsignmentDelivered"
)

// arch-test:idempotency-via-state-machine — both handlers call an Order
// transition mutator whose self/terminal guards make replay a no-op
// (AttachConsignment / MarkDelivered return without re-emitting when the order
// is already past that state); a not-yet-reached state returns
// ErrInvalidTransition, which is retryable (the event redelivers until the
// order catches up — e.g. auto-invoice completing first).

// ConsignmentCreatedSubscriber advances the Order invoiced → dispatched when
// Dispatch mints its consignment-note slot (ADR 0063 §4).
type ConsignmentCreatedSubscriber struct {
	attach command.AttachConsignmentHandler
	log    *slog.Logger
}

// NewConsignmentCreatedSubscriber wires the subscriber. log is required.
func NewConsignmentCreatedSubscriber(attach command.AttachConsignmentHandler, log *slog.Logger) *ConsignmentCreatedSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentCreatedSubscriber log required")
	}
	return &ConsignmentCreatedSubscriber{attach: attach, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_note_created.v1`.
func (h *ConsignmentCreatedSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentNoteCreatedV1) error {
	if err := h.attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID:                 tenant.ID(evt.TenantIDClaim.String()),
		OrderID:                  order.ID(evt.OrderID.String()),
		ConsignmentNoteID:        evt.ConsignmentNoteID.String(),
		TransitionedByMembership: membership.ID(evt.CreatedByMembershipID.String()),
	}); err != nil {
		return fmt.Errorf("orders consignment-created: %w", err)
	}
	return nil
}

// ConsignmentDeliveredSubscriber advances the Order dispatched → delivered when
// the carrier confirms delivery — the saga's terminal-success input (ADR 0063 §4).
type ConsignmentDeliveredSubscriber struct {
	deliver command.MarkOrderDeliveredHandler
	log     *slog.Logger
}

// NewConsignmentDeliveredSubscriber wires the subscriber. log is required.
func NewConsignmentDeliveredSubscriber(deliver command.MarkOrderDeliveredHandler, log *slog.Logger) *ConsignmentDeliveredSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentDeliveredSubscriber log required")
	}
	return &ConsignmentDeliveredSubscriber{deliver: deliver, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_delivered.v1`.
func (h *ConsignmentDeliveredSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentDeliveredV1) error {
	if err := h.deliver.Handle(ctx, command.MarkOrderDeliveredCommand{
		TenantID:                 tenant.ID(evt.TenantIDClaim.String()),
		OrderID:                  order.ID(evt.OrderID.String()),
		TransitionedByMembership: membership.ID(evt.TransitionedByMembership.String()),
	}); err != nil {
		return fmt.Errorf("orders consignment-delivered: %w", err)
	}
	return nil
}
