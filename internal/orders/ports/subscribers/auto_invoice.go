// Package subscribers holds the Orders module's inbound event subscribers —
// the saga participants per ADR 0063 §4. Typed cqrs handlers: the
// EventProcessor owns topic routing + payload decode; these are the business
// reactions only.
package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// HandlerAutoInvoice is the CI-stable handler name. DO NOT rename (renaming
// makes processed messages "fresh" against the inbox dedup table).
const HandlerAutoInvoice = "orders.subscribers.AutoInvoice"

// arch-test:idempotency-via-natural-key-precheck — InvoiceOrder's invoices.Add
// hits uq_orders_invoices_order; a replay (or a manual invoice already issued)
// surfaces invoice.ErrAlreadyExistsForOrder, which this handler treats as
// success (the number increment rolls back with the tx — gapless).

// AutoInvoiceSubscriber is the default invoicing path (ADR 0063 §4): on
// `orders.order_packed.v1` it runs InvoiceOrder so the order advances
// packed → invoiced before the Dispatch consignment-created event drives it to
// dispatched. The manual InvoiceOrder HTTP route is the alternative.
type AutoInvoiceSubscriber struct {
	invoiceOrder command.InvoiceOrderHandler
	log          *slog.Logger
}

// NewAutoInvoiceSubscriber wires the subscriber. log is required.
func NewAutoInvoiceSubscriber(invoiceOrder command.InvoiceOrderHandler, log *slog.Logger) *AutoInvoiceSubscriber {
	if log == nil {
		panic("subscribers: NewAutoInvoiceSubscriber log required")
	}
	return &AutoInvoiceSubscriber{invoiceOrder: invoiceOrder, log: log}
}

// Handle is the typed cqrs handler for `orders.order_packed.v1`.
func (h *AutoInvoiceSubscriber) Handle(ctx context.Context, evt *integrationevents.OrderPackedV1) error {
	_, err := h.invoiceOrder.Handle(ctx, command.InvoiceOrderCommand{
		TenantID:           tenant.ID(evt.TenantID),
		OrderID:            order.ID(evt.OrderID),
		IssuedByMembership: membership.ID(evt.PackedByMembershipID),
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invoice.ErrAlreadyExistsForOrder):
		// Already invoiced (replay or manual invoice) — idempotent success.
		h.log.InfoContext(ctx, "orders: auto-invoice idempotent hit", "order_id", evt.OrderID)
		return nil
	default:
		return fmt.Errorf("orders auto-invoice: %w", err)
	}
}
