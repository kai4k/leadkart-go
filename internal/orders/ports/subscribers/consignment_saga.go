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

// arch-test:idempotency-via-state-machine — every step is replay-tolerant at
// the aggregate: AttachConsignment no-ops on the same consignment at any later
// state; MarkDelivered no-ops once deliveredAt is stamped.
//
// The order is dispatched by Dispatch's own transaction, not by these handlers
// (review G2g): creating a consignment note only links it, and a delivered note
// can exist only for an order that left `invoiced` in that same transaction. A
// delivered event for an order that is not dispatched is therefore a genuine
// conflict (it errors → retry → durable DLQ: that needs a human), never something
// to repair by invoicing or dispatching from here.

// ConsignmentCreatedSubscriber links the Order to the consignment note Dispatch minted.
// It changes no state: the order is dispatched when the goods leave.
type ConsignmentCreatedSubscriber struct {
	attach command.AttachConsignmentHandler
	log    *slog.Logger
}

// NewConsignmentCreatedSubscriber wires the subscriber. log is required.
func NewConsignmentCreatedSubscriber(
	attach command.AttachConsignmentHandler,
	log *slog.Logger,
) *ConsignmentCreatedSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentCreatedSubscriber log required")
	}
	return &ConsignmentCreatedSubscriber{attach: attach, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_note_created.v1`.
func (h *ConsignmentCreatedSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentNoteCreatedV1) error {
	if err := h.attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID:          tenant.ID(evt.TenantIDClaim.String()),
		OrderID:           order.ID(evt.OrderID.String()),
		ConsignmentNoteID: evt.ConsignmentNoteID.String(),
	}); err != nil {
		return fmt.Errorf("orders consignment-created: attach: %w", err)
	}
	return nil
}

// ConsignmentDeliveredSubscriber advances the Order to delivered when the
// carrier confirms delivery — the saga's terminal-success input. It ensures the
// note is linked (a lost consignment-created event cannot strand the delivery),
// then delivers; the order must already be dispatched.
type ConsignmentDeliveredSubscriber struct {
	attach  command.AttachConsignmentHandler
	deliver command.MarkOrderDeliveredHandler
	log     *slog.Logger
}

// NewConsignmentDeliveredSubscriber wires the subscriber. log is required.
func NewConsignmentDeliveredSubscriber(
	attach command.AttachConsignmentHandler,
	deliver command.MarkOrderDeliveredHandler,
	log *slog.Logger,
) *ConsignmentDeliveredSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentDeliveredSubscriber log required")
	}
	return &ConsignmentDeliveredSubscriber{attach: attach, deliver: deliver, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_delivered.v1`.
func (h *ConsignmentDeliveredSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentDeliveredV1) error {
	tid := tenant.ID(evt.TenantIDClaim.String())
	oid := order.ID(evt.OrderID.String())
	actor := membership.ID(evt.TransitionedByMembership.String())

	if err := h.attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID:          tid,
		OrderID:           oid,
		ConsignmentNoteID: evt.ConsignmentNoteID.String(),
	}); err != nil {
		return fmt.Errorf("orders consignment-delivered: attach: %w", err)
	}
	if err := h.deliver.Handle(ctx, command.MarkOrderDeliveredCommand{
		TenantID:                 tid,
		OrderID:                  oid,
		TransitionedByMembership: actor,
	}); err != nil {
		return fmt.Errorf("orders consignment-delivered: deliver: %w", err)
	}
	return nil
}
