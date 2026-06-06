// Package ports holds the Orders module's inbound HTTP + subscriber adapters
// per ADR 0002 + the TDL hexagonal terminology canon.
//
// HTTP handlers translate request/response shapes into [app.Application]
// command + query calls. Per ADR 0046 the wire shapes here are MIRRORED in
// api/openapi.yaml — adding a route or field requires a paired spec update
// (CI-gated by the route-spec-drift test).
package ports

import "time"

// ----- Shared line item -----------------------------------------------------

// LineItemDto is one product line on a quotation / order / invoice.
type LineItemDto struct {
	ProductID     string `json:"product_id"`
	SKU           string `json:"sku"`
	Description   string `json:"description,omitzero"`
	Quantity      int32  `json:"quantity"`
	UnitMrpPaise  int64  `json:"unit_mrp_paise,omitzero"`
	UnitSalePaise int64  `json:"unit_sale_paise"`
	GstRateBps    int32  `json:"gst_rate_bps"`
}

// ----- Quotation ------------------------------------------------------------

// CreateQuotationRequest is the body for POST /quotations.
type CreateQuotationRequest struct {
	CustomerLeadID string        `json:"customer_lead_id"`
	Items          []LineItemDto `json:"items"`
	Note           string        `json:"note,omitzero"`
}

// CreateQuotationResponse returns the new quotation ID.
type CreateQuotationResponse struct {
	QuotationID string `json:"quotation_id"`
}

// ReviseQuotationRequest is the body for POST /quotations/{id}/revise.
type ReviseQuotationRequest struct {
	Items []LineItemDto `json:"items"`
	Note  string        `json:"note,omitzero"`
}

// RejectQuotationRequest is the body for POST /quotations/{id}/reject.
type RejectQuotationRequest struct {
	Reason string `json:"reason"`
}

// ApproveQuotationResponse returns the Order seeded by approval.
type ApproveQuotationResponse struct {
	OrderID string `json:"order_id"`
}

// QuotationDto is the wire shape returned by GET /quotations/{id}.
type QuotationDto struct {
	ID              string        `json:"id"`
	TenantID        string        `json:"tenant_id"`
	CustomerLeadID  string        `json:"customer_lead_id"`
	State           string        `json:"state"`
	RevisionNumber  int64         `json:"revision_number"`
	Items           []LineItemDto `json:"items"`
	RejectionReason string        `json:"rejection_reason,omitzero"`
	CreatedAt       time.Time     `json:"created_at"`
}

// ----- Order ----------------------------------------------------------------

// OrderDto is the wire shape returned by GET /orders/{id}.
type OrderDto struct {
	ID                  string        `json:"id"`
	TenantID            string        `json:"tenant_id"`
	ApprovedQuotationID string        `json:"approved_quotation_id"`
	CustomerLeadID      string        `json:"customer_lead_id"`
	State               string        `json:"state"`
	Items               []LineItemDto `json:"items"`
	SubtotalPaise       int64         `json:"subtotal_paise"`
	TaxPaise            int64         `json:"tax_paise"`
	GrandTotalPaise     int64         `json:"grand_total_paise"`
	InvoiceID           string        `json:"invoice_id,omitzero"`
	ConsignmentNoteID   string        `json:"consignment_note_id,omitzero"`
	ConfirmedAt         *time.Time    `json:"confirmed_at,omitzero"`
	PackedAt            *time.Time    `json:"packed_at,omitzero"`
	InvoicedAt          *time.Time    `json:"invoiced_at,omitzero"`
	DispatchedAt        *time.Time    `json:"dispatched_at,omitzero"`
	DeliveredAt         *time.Time    `json:"delivered_at,omitzero"`
	CompletedAt         *time.Time    `json:"completed_at,omitzero"`
	CancelledAt         *time.Time    `json:"cancelled_at,omitzero"`
	CancellationReason  string        `json:"cancellation_reason,omitzero"`
	CreatedAt           time.Time     `json:"created_at"`
}

// RecordTokenPaymentRequest is the body for POST /orders/{id}/token-payment.
type RecordTokenPaymentRequest struct {
	Method            string `json:"method"`
	AmountPaise       int64  `json:"amount_paise"`
	ExternalReference string `json:"external_reference,omitzero"`
	Notes             string `json:"notes,omitzero"`
}

// PackOrderRequest is the body for POST /orders/{id}/pack.
type PackOrderRequest struct {
	CarrierName        string     `json:"carrier_name"`
	BoxCount           int32      `json:"box_count"`
	WeightGrams        int64      `json:"weight_grams"`
	ExpectedDeliveryAt *time.Time `json:"expected_delivery_at,omitzero"`
}

// InvoiceOrderResponse returns the minted invoice + its display number.
type InvoiceOrderResponse struct {
	InvoiceID     string `json:"invoice_id"`
	NumberDisplay string `json:"number_display"`
}

// CancelOrderRequest is the body for POST /orders/{id}/cancel.
type CancelOrderRequest struct {
	Reason string `json:"reason"`
}

// ----- Payment / Invoice ----------------------------------------------------

// RecordPaymentRequest is the body for POST /orders/{id}/payments.
type RecordPaymentRequest struct {
	Kind              string     `json:"kind"`
	Method            string     `json:"method"`
	AmountPaise       int64      `json:"amount_paise"`
	ExternalReference string     `json:"external_reference,omitzero"`
	Notes             string     `json:"notes,omitzero"`
	ReceivedAt        *time.Time `json:"received_at,omitzero"`
}

// RecordPaymentResponse returns the new payment ID.
type RecordPaymentResponse struct {
	PaymentID string `json:"payment_id"`
}

// PaymentDto is one row in the payments list.
type PaymentDto struct {
	ID                string    `json:"id"`
	OrderID           string    `json:"order_id"`
	Kind              string    `json:"kind"`
	Method            string    `json:"method"`
	AmountPaise       int64     `json:"amount_paise"`
	ExternalReference string    `json:"external_reference,omitzero"`
	ReceivedAt        time.Time `json:"received_at"`
}

// ListPaymentsResponse wraps the payments list.
type ListPaymentsResponse struct {
	Payments []PaymentDto `json:"payments"`
}

// InvoiceDto is the wire shape returned by GET /orders/{id}/invoice.
type InvoiceDto struct {
	ID              string        `json:"id"`
	OrderID         string        `json:"order_id"`
	NumberDisplay   string        `json:"number_display"`
	Items           []LineItemDto `json:"items"`
	SubtotalPaise   int64         `json:"subtotal_paise"`
	TaxPaise        int64         `json:"tax_paise"`
	GrandTotalPaise int64         `json:"grand_total_paise"`
	IssuedAt        time.Time     `json:"issued_at"`
}

// errorResponse is the wire shape for non-success responses.
type errorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
