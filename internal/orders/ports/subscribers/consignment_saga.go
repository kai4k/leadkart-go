package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	dispatchevents "github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// HandlerConsignmentCreated + HandlerConsignmentDelivered are the CI-stable
// handler names for the cross-module fulfillment-saga steps. DO NOT rename.
const (
	HandlerConsignmentCreated   = "orders.subscribers.ConsignmentCreated"
	HandlerConsignmentDelivered = "orders.subscribers.ConsignmentDelivered"
)

// arch-test:idempotency-via-state-machine — every step is replay-tolerant at
// the aggregate: ensureInvoiced treats ErrAlreadyExistsForOrder as success
// (uq_orders_invoices_order natural key, allocation rolls back with the tx);
// AttachConsignment no-ops on the same consignment at any later state;
// MarkDelivered no-ops once deliveredAt is stamped.
//
// CATCH-UP CONVERGENCE (ADR 0063 §4): each saga step subsumes its
// predecessors instead of betting on broker retry budgets. The
// consignment-created handler first ensures the order is invoiced (the
// auto-invoice subscriber races it on a separate subscription), then attaches;
// the delivered handler ensures invoiced AND attached, then delivers. Any
// interleaving of the three subscriptions therefore converges
// deterministically — an event can never DLQ just because a sibling handler
// has not run yet. Genuine conflicts (e.g. order cancelled before the carrier
// delivered) still error → retry → durable DLQ, which is correct: that needs
// a human.

// ConsignmentCreatedSubscriber advances the Order to dispatched when Dispatch
// mints its consignment-note slot: ensure-invoiced → attach.
type ConsignmentCreatedSubscriber struct {
	invoiceOrder command.InvoiceOrderHandler
	attach       command.AttachConsignmentHandler
	log          *slog.Logger
}

// NewConsignmentCreatedSubscriber wires the subscriber. log is required.
func NewConsignmentCreatedSubscriber(
	invoiceOrder command.InvoiceOrderHandler,
	attach command.AttachConsignmentHandler,
	log *slog.Logger,
) *ConsignmentCreatedSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentCreatedSubscriber log required")
	}
	return &ConsignmentCreatedSubscriber{invoiceOrder: invoiceOrder, attach: attach, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_note_created.v1`.
func (h *ConsignmentCreatedSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentNoteCreatedV1) error {
	tid := tenant.ID(evt.TenantIDClaim.String())
	oid := order.ID(evt.OrderID.String())
	actor := membership.ID(evt.CreatedByMembershipID.String())

	if err := ensureInvoiced(ctx, h.invoiceOrder, h.log, tid, oid, actor); err != nil {
		return fmt.Errorf("orders consignment-created: %w", err)
	}
	if err := h.attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID:                 tid,
		OrderID:                  oid,
		ConsignmentNoteID:        evt.ConsignmentNoteID.String(),
		TransitionedByMembership: actor,
	}); err != nil {
		return fmt.Errorf("orders consignment-created: attach: %w", err)
	}
	return nil
}

// ConsignmentDeliveredSubscriber advances the Order to delivered when the
// carrier confirms delivery — the saga's terminal-success input. Full
// catch-up: ensure-invoiced → ensure-attached → deliver, so a lost or lagging
// consignment-created event cannot strand the delivery.
type ConsignmentDeliveredSubscriber struct {
	invoiceOrder command.InvoiceOrderHandler
	attach       command.AttachConsignmentHandler
	deliver      command.MarkOrderDeliveredHandler
	log          *slog.Logger
}

// NewConsignmentDeliveredSubscriber wires the subscriber. log is required.
func NewConsignmentDeliveredSubscriber(
	invoiceOrder command.InvoiceOrderHandler,
	attach command.AttachConsignmentHandler,
	deliver command.MarkOrderDeliveredHandler,
	log *slog.Logger,
) *ConsignmentDeliveredSubscriber {
	if log == nil {
		panic("subscribers: NewConsignmentDeliveredSubscriber log required")
	}
	return &ConsignmentDeliveredSubscriber{invoiceOrder: invoiceOrder, attach: attach, deliver: deliver, log: log}
}

// Handle is the typed cqrs handler for `dispatch.consignment_delivered.v1`.
func (h *ConsignmentDeliveredSubscriber) Handle(ctx context.Context, evt *dispatchevents.ConsignmentDeliveredV1) error {
	tid := tenant.ID(evt.TenantIDClaim.String())
	oid := order.ID(evt.OrderID.String())
	actor := membership.ID(evt.TransitionedByMembership.String())

	if err := ensureInvoiced(ctx, h.invoiceOrder, h.log, tid, oid, actor); err != nil {
		return fmt.Errorf("orders consignment-delivered: %w", err)
	}
	if err := h.attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID:                 tid,
		OrderID:                  oid,
		ConsignmentNoteID:        evt.ConsignmentNoteID.String(),
		TransitionedByMembership: actor,
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

// ensureInvoiced runs InvoiceOrder treating "already invoiced" as success —
// the catch-up primitive both consignment handlers share. The gapless number
// allocated inside a losing race rolls back with its tx (ADR 0063 §3), so
// numbering stays gapless.
func ensureInvoiced(
	ctx context.Context, invoiceOrder command.InvoiceOrderHandler, log *slog.Logger,
	tid tenant.ID, oid order.ID, actor membership.ID,
) error {
	_, err := invoiceOrder.Handle(ctx, command.InvoiceOrderCommand{
		TenantID:           tid,
		OrderID:            oid,
		IssuedByMembership: actor,
	})
	switch {
	case err == nil:
		log.InfoContext(ctx, "orders: saga catch-up invoiced order", "order_id", oid.String())
		return nil
	case errors.Is(err, invoice.ErrAlreadyExistsForOrder):
		return nil // normal path — the auto-invoice subscriber got there first
	default:
		return fmt.Errorf("ensure invoiced: %w", err)
	}
}
