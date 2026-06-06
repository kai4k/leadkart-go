package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
)

// RecordPaymentCommand records a balance / full / refund receipt against an
// order (the token receipt has its own command that also advances the order).
// Append-only — no order transition.
type RecordPaymentCommand struct {
	TenantID             tenant.ID
	OrderID              order.ID
	Kind                 string
	Method               string
	AmountPaise          int64
	ExternalReference    string
	Notes                string
	ReceivedAt           time.Time
	RecordedByMembership membership.ID
}

// RecordPaymentResult returns the new payment ID.
type RecordPaymentResult struct {
	PaymentID payment.ID
}

// RecordPaymentHandler records a payment receipt.
type RecordPaymentHandler struct {
	payments     payment.Repository
	now          func() time.Time
	newPaymentID func() payment.ID
}

// NewRecordPaymentHandler wires the handler.
func NewRecordPaymentHandler(payments payment.Repository, now func() time.Time, newPaymentID func() payment.ID) RecordPaymentHandler {
	if now == nil {
		now = time.Now
	}
	return RecordPaymentHandler{payments: payments, now: now, newPaymentID: newPaymentID}
}

// Handle records the payment, returning [payment.ErrAlreadyExistsForExternalReference]
// on a duplicate external reference (webhook idempotency).
func (h RecordPaymentHandler) Handle(ctx context.Context, cmd RecordPaymentCommand) (RecordPaymentResult, error) {
	if cmd.TenantID == "" {
		return RecordPaymentResult{}, errors.New("orders record_payment: tenant id required")
	}
	received := cmd.ReceivedAt
	if received.IsZero() {
		received = h.now().UTC()
	}
	p, err := payment.New(payment.NewInput{
		ID:                   h.newPaymentID(),
		TenantID:             cmd.TenantID,
		OrderID:              cmd.OrderID,
		Kind:                 payment.Kind(cmd.Kind),
		Method:               payment.Method(cmd.Method),
		AmountPaise:          cmd.AmountPaise,
		ExternalReference:    cmd.ExternalReference,
		Notes:                cmd.Notes,
		ReceivedAt:           received.UTC(),
		RecordedAt:           h.now().UTC(),
		RecordedByMembership: cmd.RecordedByMembership,
	})
	if err != nil {
		return RecordPaymentResult{}, fmt.Errorf("orders record_payment: %w", err)
	}
	if err := h.payments.Add(ctx, p); err != nil {
		return RecordPaymentResult{}, fmt.Errorf("orders record_payment: %w", err)
	}
	return RecordPaymentResult{PaymentID: p.ID()}, nil
}
