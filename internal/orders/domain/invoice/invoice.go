// Package invoice owns the [Invoice] aggregate — the immutable tax document
// issued after an order is packed (BRD §6.4 + §A-014 + ADR 0063 §3).
//
// Key invariants:
//   - APPEND-ONLY: no mutator; only New + UnmarshalFromDB.
//   - GAPLESS NUMBERING: invoice_number allocated by [invoicenumber.Allocator]
//     inside the same tx so rollback reverts the counter — never via Postgres
//     SEQUENCE nextval(), which burns numbers on rollback and breaks GSTR-1.
//   - TENANT-SCOPED: composite PK (tenant_id, id); RLS+FORCE.
//
// The command handler allocates the Number and passes it in; Invoice never
// calls Allocator directly (allocation must be in the same tx as Insert).
package invoice

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ErrInvalid is the sentinel for ctor invariant violations. Map to HTTP 422.
var ErrInvalid = errors.New("invoice: invalid")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String implements [fmt.Stringer].
func (id ID) String() string { return string(id) }

// TaxLine is the per-HSN tax breakdown row — one entry per distinct
// (HSNCode, GSTRateBps) pair. Required for GSTR-1 export.
type TaxLine struct {
	HSNCode           string
	GSTRateBps        int32 // basis points (12% = 1200) per inventory canon
	TaxableValuePaise int64
	TaxAmountPaise    int64
}

// Validate runs the per-line invariants.
func (tl TaxLine) Validate() error {
	if strings.TrimSpace(tl.HSNCode) == "" {
		return fmt.Errorf("%w: tax_line.hsn_code required", ErrInvalid)
	}
	if tl.GSTRateBps < 0 || tl.GSTRateBps > 10000 {
		return fmt.Errorf("%w: tax_line.gst_rate_bps must be 0..10000 (got %d)", ErrInvalid, tl.GSTRateBps)
	}
	if tl.TaxableValuePaise < 0 {
		return fmt.Errorf("%w: tax_line.taxable_value_paise must be >= 0", ErrInvalid)
	}
	if tl.TaxAmountPaise < 0 {
		return fmt.Errorf("%w: tax_line.tax_amount_paise must be >= 0", ErrInvalid)
	}
	return nil
}

// Invoice is the aggregate root. Append-only.
type Invoice struct {
	id                   ID
	tenantID             tenant.ID
	orderID              order.ID
	number               invoicenumber.Number
	lineItems            []quotation.LineItem
	taxLines             []TaxLine
	subtotalPaise        int64
	taxPaise             int64
	grandTotalPaise      int64
	issuedAt             time.Time
	issuedByMembershipID membership.ID
}

// NewInput carries all fields for [New]. Assembled by the command handler
// after allocating the Number via [invoicenumber.Allocator].
type NewInput struct {
	ID                   ID
	TenantID             tenant.ID
	OrderID              order.ID
	Number               invoicenumber.Number
	LineItems            []quotation.LineItem
	TaxLines             []TaxLine
	SubtotalPaise        int64
	TaxPaise             int64
	GrandTotalPaise      int64
	IssuedAt             time.Time
	IssuedByMembershipID membership.ID
}

// New constructs an Invoice. Validates every invariant.
func New(in NewInput) (*Invoice, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: order_id required", ErrInvalid)
	}
	if in.Number.IsZero() {
		return nil, fmt.Errorf("%w: invoice_number required (allocate via invoicenumber.Allocator)", ErrInvalid)
	}
	if in.Number.Kind() != invoicenumber.KindInvoice {
		return nil, fmt.Errorf("%w: invoice_number.kind must be %s (got %s)",
			ErrInvalid, invoicenumber.KindInvoice, in.Number.Kind())
	}
	if len(in.LineItems) == 0 {
		return nil, fmt.Errorf("%w: line_items must be non-empty", ErrInvalid)
	}
	for i := range in.LineItems {
		if err := in.LineItems[i].Validate(); err != nil {
			return nil, fmt.Errorf("%w: item[%d]: %w", ErrInvalid, i, err)
		}
	}
	for i := range in.TaxLines {
		if err := in.TaxLines[i].Validate(); err != nil {
			return nil, fmt.Errorf("%w: tax_line[%d]: %w", ErrInvalid, i, err)
		}
	}
	if in.SubtotalPaise < 0 {
		return nil, fmt.Errorf("%w: subtotal_paise must be >= 0", ErrInvalid)
	}
	if in.TaxPaise < 0 {
		return nil, fmt.Errorf("%w: tax_paise must be >= 0", ErrInvalid)
	}
	if in.GrandTotalPaise < 0 {
		return nil, fmt.Errorf("%w: grand_total_paise must be >= 0", ErrInvalid)
	}
	if in.GrandTotalPaise != in.SubtotalPaise+in.TaxPaise {
		return nil, fmt.Errorf("%w: grand_total_paise (%d) must equal subtotal (%d) + tax (%d)",
			ErrInvalid, in.GrandTotalPaise, in.SubtotalPaise, in.TaxPaise)
	}
	if in.IssuedAt.IsZero() {
		return nil, fmt.Errorf("%w: issued_at required", ErrInvalid)
	}
	if in.IssuedByMembershipID == "" {
		return nil, fmt.Errorf("%w: issued_by_membership_id required", ErrInvalid)
	}

	items := make([]quotation.LineItem, len(in.LineItems))
	copy(items, in.LineItems)
	taxes := make([]TaxLine, len(in.TaxLines))
	copy(taxes, in.TaxLines)

	return &Invoice{
		id:                   in.ID,
		tenantID:             in.TenantID,
		orderID:              in.OrderID,
		number:               in.Number,
		lineItems:            items,
		taxLines:             taxes,
		subtotalPaise:        in.SubtotalPaise,
		taxPaise:             in.TaxPaise,
		grandTotalPaise:      in.GrandTotalPaise,
		issuedAt:             in.IssuedAt,
		issuedByMembershipID: in.IssuedByMembershipID,
	}, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                   ID
	TenantID             tenant.ID
	OrderID              order.ID
	Number               invoicenumber.Number
	LineItems            []quotation.LineItem
	TaxLines             []TaxLine
	SubtotalPaise        int64
	TaxPaise             int64
	GrandTotalPaise      int64
	IssuedAt             time.Time
	IssuedByMembershipID membership.ID
}

// UnmarshalFromDB rehydrates the aggregate without re-validating.
func UnmarshalFromDB(s Snapshot) *Invoice {
	items := make([]quotation.LineItem, len(s.LineItems))
	copy(items, s.LineItems)
	taxes := make([]TaxLine, len(s.TaxLines))
	copy(taxes, s.TaxLines)
	return &Invoice{
		id:                   s.ID,
		tenantID:             s.TenantID,
		orderID:              s.OrderID,
		number:               s.Number,
		lineItems:            items,
		taxLines:             taxes,
		subtotalPaise:        s.SubtotalPaise,
		taxPaise:             s.TaxPaise,
		grandTotalPaise:      s.GrandTotalPaise,
		issuedAt:             s.IssuedAt,
		issuedByMembershipID: s.IssuedByMembershipID,
	}
}

// ID returns the aggregate identity.
func (inv *Invoice) ID() ID { return inv.id }

// TenantID returns the owning tenant.
func (inv *Invoice) TenantID() tenant.ID { return inv.tenantID }

// OrderID returns the source order.
func (inv *Invoice) OrderID() order.ID { return inv.orderID }

// Number returns the gapless invoice number.
func (inv *Invoice) Number() invoicenumber.Number { return inv.number }

// LineItems returns a defensive copy of the frozen items snapshot.
func (inv *Invoice) LineItems() []quotation.LineItem {
	out := make([]quotation.LineItem, len(inv.lineItems))
	copy(out, inv.lineItems)
	return out
}

// TaxLines returns a defensive copy of the per-HSN tax breakdown.
func (inv *Invoice) TaxLines() []TaxLine {
	out := make([]TaxLine, len(inv.taxLines))
	copy(out, inv.taxLines)
	return out
}

// SubtotalPaise returns the sum of taxable values in paise.
func (inv *Invoice) SubtotalPaise() int64 { return inv.subtotalPaise }

// TaxPaise returns the total tax in paise.
func (inv *Invoice) TaxPaise() int64 { return inv.taxPaise }

// GrandTotalPaise returns subtotal + tax in paise.
func (inv *Invoice) GrandTotalPaise() int64 { return inv.grandTotalPaise }

// IssuedAt returns the issue timestamp.
func (inv *Invoice) IssuedAt() time.Time { return inv.issuedAt }

// IssuedByMembershipID returns the membership that issued this invoice.
func (inv *Invoice) IssuedByMembershipID() membership.ID { return inv.issuedByMembershipID }
