// Package creditnote owns the [CreditNote] aggregate — the immutable
// financial reversal document issued when an order is cancelled
// POST-DELIVERY (per BRD §6.4 + §A-014 + ADR 0063 §3).
//
// Two flavours, both modelled here, distinguished by [Kind]:
//
//   - "credit_note" — post-delivery reversal (`CDN/...` prefix).
//     Issued when a customer returns goods OR claims a billing error
//     against an already-delivered invoice.
//   - "cancellation_note" — pre-delivery cancellation (`CN/...` prefix).
//     Issued when an order is cancelled AFTER invoicing but BEFORE
//     dispatch.
//
// Both write to the SAME orders.credit_notes table — the Kind
// discriminator + the separate `invoicenumber.Allocator` sequences
// keep them numerically distinct. (Modelling them as one aggregate
// keeps the audit query "show all reversals for this invoice" simple.)
//
// Invariants:
//
//   - APPEND-ONLY. Once issued, never updated.
//   - Each CreditNote references EXACTLY ONE Invoice. Partial
//     reversals (multiple CDNs against one invoice) are permitted —
//     the partial unique index on (tenant_id, invoice_id, kind=cancellation_note)
//     prevents a SECOND cancellation_note but allows multiple credit_notes
//     for return scenarios.
package creditnote

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// ErrInvalid is the sentinel for ctor invariant violations.
var ErrInvalid = errors.New("creditnote: invalid")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// CreditNote is the aggregate root. Append-only.
type CreditNote struct {
	id                 ID
	tenantID           tenant.ID
	invoiceID          invoice.ID
	number             invoicenumber.Number
	kind               invoicenumber.Kind // credit_note OR cancellation_note
	reason             string
	amountPaise        int64
	issuedAt           time.Time
	issuedByMembership membership.ID
}

// NewInput is the ctor input.
type NewInput struct {
	ID                 ID
	TenantID           tenant.ID
	InvoiceID          invoice.ID
	Number             invoicenumber.Number
	Kind               invoicenumber.Kind // KindCreditNote or KindCancellationNote
	Reason             string
	AmountPaise        int64
	IssuedAt           time.Time
	IssuedByMembership membership.ID
}

// New constructs a CreditNote. Validates every invariant.
func New(in NewInput) (*CreditNote, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("%w: invoice_id required", ErrInvalid)
	}
	if in.Number.IsZero() {
		return nil, fmt.Errorf("%w: number required (allocate via invoicenumber.Allocator)", ErrInvalid)
	}
	if in.Kind != invoicenumber.KindCreditNote && in.Kind != invoicenumber.KindCancellationNote {
		return nil, fmt.Errorf("%w: kind must be credit_note or cancellation_note (got %s)",
			ErrInvalid, in.Kind)
	}
	if in.Number.Kind() != in.Kind {
		return nil, fmt.Errorf("%w: number.kind (%s) must match Kind (%s)",
			ErrInvalid, in.Number.Kind(), in.Kind)
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason required", ErrInvalid)
	}
	if in.AmountPaise <= 0 {
		return nil, fmt.Errorf("%w: amount_paise must be positive (got %d)", ErrInvalid, in.AmountPaise)
	}
	if in.IssuedAt.IsZero() {
		return nil, fmt.Errorf("%w: issued_at required", ErrInvalid)
	}
	if in.IssuedByMembership == "" {
		return nil, fmt.Errorf("%w: issued_by_membership required", ErrInvalid)
	}
	return &CreditNote{
		id:                 in.ID,
		tenantID:           in.TenantID,
		invoiceID:          in.InvoiceID,
		number:             in.Number,
		kind:               in.Kind,
		reason:             reason,
		amountPaise:        in.AmountPaise,
		issuedAt:           in.IssuedAt,
		issuedByMembership: in.IssuedByMembership,
	}, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                 ID
	TenantID           tenant.ID
	InvoiceID          invoice.ID
	Number             invoicenumber.Number
	Kind               invoicenumber.Kind
	Reason             string
	AmountPaise        int64
	IssuedAt           time.Time
	IssuedByMembership membership.ID
}

// UnmarshalFromDB rehydrates the aggregate without re-validating.
func UnmarshalFromDB(s Snapshot) *CreditNote {
	return &CreditNote{
		id:                 s.ID,
		tenantID:           s.TenantID,
		invoiceID:          s.InvoiceID,
		number:             s.Number,
		kind:               s.Kind,
		reason:             s.Reason,
		amountPaise:        s.AmountPaise,
		issuedAt:           s.IssuedAt,
		issuedByMembership: s.IssuedByMembership,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (c *CreditNote) ID() ID { return c.id }

// TenantID returns the owning tenant.
func (c *CreditNote) TenantID() tenant.ID { return c.tenantID }

// InvoiceID returns the source invoice.
func (c *CreditNote) InvoiceID() invoice.ID { return c.invoiceID }

// Number returns the gapless display number.
func (c *CreditNote) Number() invoicenumber.Number { return c.number }

// Kind returns the discriminator (credit_note OR cancellation_note).
func (c *CreditNote) Kind() invoicenumber.Kind { return c.kind }

// Reason returns the operator-supplied reason.
func (c *CreditNote) Reason() string { return c.reason }

// AmountPaise returns the reversal amount.
func (c *CreditNote) AmountPaise() int64 { return c.amountPaise }

// IssuedAt returns the issue timestamp.
func (c *CreditNote) IssuedAt() time.Time { return c.issuedAt }

// IssuedByMembership returns the actor.
func (c *CreditNote) IssuedByMembership() membership.ID { return c.issuedByMembership }
