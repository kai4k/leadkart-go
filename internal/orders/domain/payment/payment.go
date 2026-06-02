// Package payment owns the [Payment] aggregate: the immutable receipt for a
// single payment event against an order (BRD §6.4, ADR 0063).
//
// Kinds (see [Kind]): "token" (upfront deposit, BRD §6.4 step 4), "full"
// (balance at completion, step 10), "refund" (returned post-cancellation,
// linked to a [creditnote.CreditNote] for the audit chain).
//
// Invariants:
//   - Append-only. Each payment is its own row; Order.paid_amount is the
//     derived sum of token + full minus refunds.
//   - Idempotency via the (tenant_id, external_reference) partial unique index
//     when ExternalReference is set — guards against double-recording the same
//     real-world payment (e.g. retried webhook). Empty ExternalReference
//     (manual entry) bypasses the check; the operator is trusted.
package payment

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrInvalid is the sentinel for ctor invariant violations.
var ErrInvalid = errors.New("payment: invalid")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// Kind tags the payment class.
type Kind string

// Closed catalogue.
const (
	// KindToken — upfront deposit (typically 10-30% of grand_total).
	KindToken Kind = "token"
	// KindFull — balance payment at completion.
	KindFull Kind = "full"
	// KindRefund — refund post-cancellation. AmountPaise is positive; the sign
	// in the derived paid_amount is applied at query time.
	KindRefund Kind = "refund"
)

// String returns the wire form.
func (k Kind) String() string { return string(k) }

// IsValid reports whether k is a known catalogue entry.
func (k Kind) IsValid() bool {
	switch k {
	case KindToken, KindFull, KindRefund:
		return true
	}
	return false
}

// Method is the payment instrument used.
type Method string

// Open catalogue — wire-stable strings. Persisted as text with a CHECK
// constraint; new values land via migration.
const (
	MethodUPI         Method = "upi"
	MethodNEFT        Method = "neft"
	MethodRTGS        Method = "rtgs"
	MethodIMPS        Method = "imps"
	MethodCheque      Method = "cheque"
	MethodCash        Method = "cash"
	MethodCardOffline Method = "card_offline"
)

// String returns the wire form.
func (m Method) String() string { return string(m) }

// IsValid reports whether m is a known catalogue entry.
func (m Method) IsValid() bool {
	switch m {
	case MethodUPI, MethodNEFT, MethodRTGS, MethodIMPS, MethodCheque, MethodCash, MethodCardOffline:
		return true
	}
	return false
}

// Payment is the aggregate root. Append-only.
type Payment struct {
	id                   ID
	tenantID             tenant.ID
	orderID              order.ID
	kind                 Kind
	method               Method
	amountPaise          int64
	externalReference    string
	notes                string
	receivedAt           time.Time
	recordedAt           time.Time
	recordedByMembership membership.ID
}

// NewInput is the ctor input.
type NewInput struct {
	ID                   ID
	TenantID             tenant.ID
	OrderID              order.ID
	Kind                 Kind
	Method               Method
	AmountPaise          int64
	ExternalReference    string // optional; bank UTR / UPI ref / cheque no.
	Notes                string
	ReceivedAt           time.Time
	RecordedAt           time.Time
	RecordedByMembership membership.ID
}

// New constructs a Payment.
func New(in NewInput) (*Payment, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: order_id required", ErrInvalid)
	}
	if !in.Kind.IsValid() {
		return nil, fmt.Errorf("%w: kind %q not in catalogue", ErrInvalid, in.Kind)
	}
	if !in.Method.IsValid() {
		return nil, fmt.Errorf("%w: method %q not in catalogue", ErrInvalid, in.Method)
	}
	if in.AmountPaise <= 0 {
		return nil, fmt.Errorf("%w: amount_paise must be positive (got %d)", ErrInvalid, in.AmountPaise)
	}
	if in.ReceivedAt.IsZero() {
		return nil, fmt.Errorf("%w: received_at required", ErrInvalid)
	}
	if in.RecordedAt.IsZero() {
		return nil, fmt.Errorf("%w: recorded_at required", ErrInvalid)
	}
	if in.RecordedAt.Before(in.ReceivedAt) {
		return nil, fmt.Errorf("%w: recorded_at (%s) must be >= received_at (%s)",
			ErrInvalid, in.RecordedAt, in.ReceivedAt)
	}
	if in.RecordedByMembership == "" {
		return nil, fmt.Errorf("%w: recorded_by_membership required", ErrInvalid)
	}
	return &Payment{
		id:                   in.ID,
		tenantID:             in.TenantID,
		orderID:              in.OrderID,
		kind:                 in.Kind,
		method:               in.Method,
		amountPaise:          in.AmountPaise,
		externalReference:    strings.TrimSpace(in.ExternalReference),
		notes:                strings.TrimSpace(in.Notes),
		receivedAt:           in.ReceivedAt,
		recordedAt:           in.RecordedAt,
		recordedByMembership: in.RecordedByMembership,
	}, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                   ID
	TenantID             tenant.ID
	OrderID              order.ID
	Kind                 Kind
	Method               Method
	AmountPaise          int64
	ExternalReference    string
	Notes                string
	ReceivedAt           time.Time
	RecordedAt           time.Time
	RecordedByMembership membership.ID
}

// UnmarshalFromDB rehydrates the aggregate.
func UnmarshalFromDB(s Snapshot) *Payment {
	return &Payment{
		id:                   s.ID,
		tenantID:             s.TenantID,
		orderID:              s.OrderID,
		kind:                 s.Kind,
		method:               s.Method,
		amountPaise:          s.AmountPaise,
		externalReference:    s.ExternalReference,
		notes:                s.Notes,
		receivedAt:           s.ReceivedAt,
		recordedAt:           s.RecordedAt,
		recordedByMembership: s.RecordedByMembership,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (p *Payment) ID() ID { return p.id }

// TenantID returns the owning tenant.
func (p *Payment) TenantID() tenant.ID { return p.tenantID }

// OrderID returns the linked order.
func (p *Payment) OrderID() order.ID { return p.orderID }

// Kind returns the payment class.
func (p *Payment) Kind() Kind { return p.kind }

// Method returns the payment instrument.
func (p *Payment) Method() Method { return p.method }

// AmountPaise returns the absolute payment amount.
func (p *Payment) AmountPaise() int64 { return p.amountPaise }

// ExternalReference returns the operator-supplied external ref (UTR / UPI ref /
// cheque no), or empty.
func (p *Payment) ExternalReference() string { return p.externalReference }

// Notes returns the operator-supplied free-form note.
func (p *Payment) Notes() string { return p.notes }

// ReceivedAt returns the real-world payment timestamp (bank-side).
func (p *Payment) ReceivedAt() time.Time { return p.receivedAt }

// RecordedAt returns the in-system record timestamp.
func (p *Payment) RecordedAt() time.Time { return p.recordedAt }

// RecordedByMembership returns the operator.
func (p *Payment) RecordedByMembership() membership.ID { return p.recordedByMembership }
