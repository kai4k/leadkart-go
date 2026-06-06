// Package query holds Orders read-side handlers per TDL canon. Strict CQRS
// (ADR 0067): every handler returns a flat *View / []View read model, never a
// domain aggregate; the port maps the View straight to its wire DTO.
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ErrOrderNotFound / ErrQuotationNotFound / ErrInvoiceNotFound surface when no
// row exists in the caller's tenant scope.
var (
	ErrOrderNotFound     = errors.New("orders query: order not found")
	ErrQuotationNotFound = errors.New("orders query: quotation not found")
	ErrInvoiceNotFound   = errors.New("orders query: invoice not found")
)

// ----- Read models ----------------------------------------------------------

// LineItemView is the read shape of one order/invoice line.
type LineItemView struct {
	ProductID     string
	SKU           string
	Description   string
	Quantity      int32
	UnitMrpPaise  int64
	UnitSalePaise int64
	GstRateBps    int32
}

// OrderView is the read projection of an Order aggregate.
type OrderView struct {
	ID                  string
	TenantID            string
	ApprovedQuotationID string
	CustomerLeadID      string
	State               string
	Items               []LineItemView
	SubtotalPaise       int64
	TaxPaise            int64
	GrandTotalPaise     int64
	InvoiceID           string
	ConsignmentNoteID   string
	ConfirmedAt         *time.Time
	PackedAt            *time.Time
	InvoicedAt          *time.Time
	DispatchedAt        *time.Time
	DeliveredAt         *time.Time
	CompletedAt         *time.Time
	CancelledAt         *time.Time
	CancellationReason  string
	CreatedAt           time.Time
}

// QuotationView is the read projection of a Quotation aggregate (current
// revision only; full history is an admin concern, not the default read).
type QuotationView struct {
	ID              string
	TenantID        string
	CustomerLeadID  string
	State           string
	RevisionNumber  int64
	Items           []LineItemView
	RejectionReason string
	CreatedAt       time.Time
}

// InvoiceView is the read projection of an Invoice aggregate.
type InvoiceView struct {
	ID              string
	OrderID         string
	NumberDisplay   string
	Items           []LineItemView
	SubtotalPaise   int64
	TaxPaise        int64
	GrandTotalPaise int64
	IssuedAt        time.Time
}

// PaymentView is the read projection of a Payment aggregate.
type PaymentView struct {
	ID                string
	OrderID           string
	Kind              string
	Method            string
	AmountPaise       int64
	ExternalReference string
	ReceivedAt        time.Time
}

func lineItemsView(items []quotation.LineItem) []LineItemView {
	out := make([]LineItemView, len(items))
	for i, li := range items {
		out[i] = LineItemView{
			ProductID: li.ProductID, SKU: li.SKU, Description: li.Description,
			Quantity: li.Quantity, UnitMrpPaise: li.UnitMrpPaise,
			UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
		}
	}
	return out
}

// ----- GetOrder -------------------------------------------------------------

// GetOrderQuery selects a single order by ID under tenant scope.
type GetOrderQuery struct {
	TenantID tenant.ID
	OrderID  order.ID
}

// GetOrderHandler runs the single-order read.
type GetOrderHandler struct{ orders order.Repository }

// NewGetOrderHandler wires the handler.
func NewGetOrderHandler(orders order.Repository) GetOrderHandler {
	if orders == nil {
		panic("query: NewGetOrderHandler orders required")
	}
	return GetOrderHandler{orders: orders}
}

// Handle returns the order read model or [ErrOrderNotFound].
func (h GetOrderHandler) Handle(ctx context.Context, q GetOrderQuery) (*OrderView, error) {
	o, err := h.orders.GetByID(ctx, q.TenantID, q.OrderID)
	if err != nil {
		if errors.Is(err, order.ErrNotFound) {
			return nil, ErrOrderNotFound
		}
		return nil, fmt.Errorf("orders get_order: %w", err)
	}
	v := OrderView{
		ID:                  o.ID().String(),
		TenantID:            o.TenantID().String(),
		ApprovedQuotationID: o.ApprovedQuotationID().String(),
		CustomerLeadID:      o.CustomerLeadID().String(),
		State:               o.State().String(),
		Items:               lineItemsView(o.ConfirmedItems()),
		SubtotalPaise:       o.SubtotalPaise(),
		TaxPaise:            o.TaxPaise(),
		GrandTotalPaise:     o.GrandTotalPaise(),
		InvoiceID:           o.InvoiceID(),
		ConsignmentNoteID:   o.ConsignmentNoteID(),
		ConfirmedAt:         o.ConfirmedAt(),
		PackedAt:            o.PackedAt(),
		InvoicedAt:          o.InvoicedAt(),
		DispatchedAt:        o.DispatchedAt(),
		DeliveredAt:         o.DeliveredAt(),
		CompletedAt:         o.CompletedAt(),
		CancelledAt:         o.CancelledAt(),
		CancellationReason:  o.CancellationReason(),
		CreatedAt:           o.CreatedAt(),
	}
	return &v, nil
}

// ----- GetQuotation ---------------------------------------------------------

// GetQuotationQuery selects a single quotation by ID under tenant scope.
type GetQuotationQuery struct {
	TenantID    tenant.ID
	QuotationID quotation.ID
}

// GetQuotationHandler runs the single-quotation read.
type GetQuotationHandler struct{ quotations quotation.Repository }

// NewGetQuotationHandler wires the handler.
func NewGetQuotationHandler(quotations quotation.Repository) GetQuotationHandler {
	if quotations == nil {
		panic("query: NewGetQuotationHandler quotations required")
	}
	return GetQuotationHandler{quotations: quotations}
}

// Handle returns the quotation read model or [ErrQuotationNotFound].
func (h GetQuotationHandler) Handle(ctx context.Context, q GetQuotationQuery) (*QuotationView, error) {
	quo, err := h.quotations.GetByID(ctx, q.TenantID, q.QuotationID)
	if err != nil {
		if errors.Is(err, quotation.ErrNotFound) {
			return nil, ErrQuotationNotFound
		}
		return nil, fmt.Errorf("orders get_quotation: %w", err)
	}
	rev := quo.CurrentRevision()
	v := QuotationView{
		ID:              quo.ID().String(),
		TenantID:        quo.TenantID().String(),
		CustomerLeadID:  quo.CustomerLeadID().String(),
		State:           quo.State().String(),
		RevisionNumber:  rev.Number,
		Items:           lineItemsView(rev.Items),
		RejectionReason: quo.RejectionReason(),
		CreatedAt:       quo.CreatedAt(),
	}
	return &v, nil
}

// ----- GetInvoiceByOrder ----------------------------------------------------

// GetInvoiceByOrderQuery selects the invoice attached to an order.
type GetInvoiceByOrderQuery struct {
	TenantID tenant.ID
	OrderID  order.ID
}

// GetInvoiceByOrderHandler runs the by-order invoice read.
type GetInvoiceByOrderHandler struct{ invoices invoice.Repository }

// NewGetInvoiceByOrderHandler wires the handler.
func NewGetInvoiceByOrderHandler(invoices invoice.Repository) GetInvoiceByOrderHandler {
	if invoices == nil {
		panic("query: NewGetInvoiceByOrderHandler invoices required")
	}
	return GetInvoiceByOrderHandler{invoices: invoices}
}

// Handle returns the invoice read model or [ErrInvoiceNotFound].
func (h GetInvoiceByOrderHandler) Handle(ctx context.Context, q GetInvoiceByOrderQuery) (*InvoiceView, error) {
	inv, err := h.invoices.GetByOrderID(ctx, q.TenantID, q.OrderID)
	if err != nil {
		if errors.Is(err, invoice.ErrNotFound) {
			return nil, ErrInvoiceNotFound
		}
		return nil, fmt.Errorf("orders get_invoice_by_order: %w", err)
	}
	v := InvoiceView{
		ID:              inv.ID().String(),
		OrderID:         inv.OrderID().String(),
		NumberDisplay:   inv.Number().String(),
		Items:           lineItemsView(inv.LineItems()),
		SubtotalPaise:   inv.SubtotalPaise(),
		TaxPaise:        inv.TaxPaise(),
		GrandTotalPaise: inv.GrandTotalPaise(),
		IssuedAt:        inv.IssuedAt(),
	}
	return &v, nil
}

// ----- ListPaymentsByOrder --------------------------------------------------

// ListPaymentsByOrderQuery selects an order's payments in receipt order.
type ListPaymentsByOrderQuery struct {
	TenantID tenant.ID
	OrderID  order.ID
}

// ListPaymentsByOrderHandler runs the payments read.
type ListPaymentsByOrderHandler struct{ payments payment.Repository }

// NewListPaymentsByOrderHandler wires the handler.
func NewListPaymentsByOrderHandler(payments payment.Repository) ListPaymentsByOrderHandler {
	if payments == nil {
		panic("query: NewListPaymentsByOrderHandler payments required")
	}
	return ListPaymentsByOrderHandler{payments: payments}
}

// Handle returns the order's payment read models.
func (h ListPaymentsByOrderHandler) Handle(ctx context.Context, q ListPaymentsByOrderQuery) ([]PaymentView, error) {
	ps, err := h.payments.ListByOrder(ctx, q.TenantID, q.OrderID)
	if err != nil {
		return nil, fmt.Errorf("orders list_payments: %w", err)
	}
	out := make([]PaymentView, 0, len(ps))
	for _, p := range ps {
		out = append(out, PaymentView{
			ID:                p.ID().String(),
			OrderID:           p.OrderID().String(),
			Kind:              p.Kind().String(),
			Method:            p.Method().String(),
			AmountPaise:       p.AmountPaise(),
			ExternalReference: p.ExternalReference(),
			ReceivedAt:        p.ReceivedAt(),
		})
	}
	return out, nil
}
