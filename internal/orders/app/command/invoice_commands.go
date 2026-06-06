package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// InvoiceOrderCommand advances packed → invoiced: it allocates a gapless
// invoice number, mints the Invoice from the order's frozen snapshot, and links
// it back onto the Order — all in ONE UoW tx so the number, the invoice row,
// and the order transition commit (or roll back) together (ADR 0063 §3).
type InvoiceOrderCommand struct {
	TenantID           tenant.ID
	OrderID            order.ID
	IssuedByMembership membership.ID
}

// InvoiceOrderResult returns the new invoice ID + its display number.
type InvoiceOrderResult struct {
	InvoiceID     invoice.ID
	NumberDisplay string
}

// InvoiceOrderHandler runs the invoicing flow.
type InvoiceOrderHandler struct {
	uow          pg.UnitOfWork
	orders       order.Repository
	invoices     invoice.Repository
	allocator    invoicenumber.Allocator
	now          func() time.Time
	newInvoiceID func() invoice.ID
}

// NewInvoiceOrderHandler wires the handler.
func NewInvoiceOrderHandler(
	uow pg.UnitOfWork, orders order.Repository, invoices invoice.Repository,
	allocator invoicenumber.Allocator, now func() time.Time, newInvoiceID func() invoice.ID,
) InvoiceOrderHandler {
	if now == nil {
		now = time.Now
	}
	return InvoiceOrderHandler{uow: uow, orders: orders, invoices: invoices, allocator: allocator, now: now, newInvoiceID: newInvoiceID}
}

// Handle mints the invoice + attaches it, atomically.
func (h InvoiceOrderHandler) Handle(ctx context.Context, cmd InvoiceOrderCommand) (InvoiceOrderResult, error) {
	if cmd.TenantID == "" {
		return InvoiceOrderResult{}, errors.New("orders invoice_order: tenant id required")
	}
	var result InvoiceOrderResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		now := h.now().UTC()
		return h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
			num, err := h.allocator.Allocate(ctx, cmd.TenantID, invoicenumber.FromDate(now), invoicenumber.KindInvoice)
			if err != nil {
				return false, fmt.Errorf("allocate number: %w", err)
			}
			inv, err := invoice.New(invoice.NewInput{
				ID:                   h.newInvoiceID(),
				TenantID:             cmd.TenantID,
				OrderID:              o.ID(),
				Number:               num,
				LineItems:            o.ConfirmedItems(),
				SubtotalPaise:        o.SubtotalPaise(),
				TaxPaise:             o.TaxPaise(),
				GrandTotalPaise:      o.GrandTotalPaise(),
				IssuedAt:             now,
				IssuedByMembershipID: cmd.IssuedByMembership,
			})
			if err != nil {
				return false, fmt.Errorf("construct invoice: %w", err)
			}
			if err := h.invoices.Add(ctx, inv); err != nil {
				return false, fmt.Errorf("add invoice: %w", err)
			}
			if err := o.AttachInvoice(inv.ID().String(), cmd.IssuedByMembership, now); err != nil {
				return false, fmt.Errorf("attach invoice: %w", err)
			}
			result = InvoiceOrderResult{InvoiceID: inv.ID(), NumberDisplay: num.String()}
			return true, nil
		})
	})
	if err != nil {
		return InvoiceOrderResult{}, fmt.Errorf("orders invoice_order: %w", err)
	}
	return result, nil
}
