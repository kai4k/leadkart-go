// Package order owns the [Order] aggregate — the state machine that walks an
// approved [quotation.Quotation] from confirmation through delivery (BRD §6.4,
// ADR 0063).
//
// Order is the load-bearing state machine of the fulfillment saga: subscribers
// to its integration events drive inventory reservations, dispatch-note
// creation, and notification fan-out. Its state column IS the saga state
// (ADR 0063 §4).
//
// Items snapshot: on QuotationApproved the Order freezes a copy of the approved
// revision's items (confirmed_items) and carries it through the lifecycle, so
// post-approval Quotation revisions (a fresh draft for the NEXT order) don't
// affect the in-flight Order.
//
// Money: every monetary field is int64 paise. Totals are derived from the items
// snapshot at ctor/transition time and stored on the row for read-side
// convenience. Per ADR 0061 (Stripe canon): never float, never decimal.
package order

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ErrInvalid is the sentinel for ctor / input invariant violations. Map to HTTP 422.
var ErrInvalid = errors.New("order: invalid")

// ErrInvalidTransition is returned for a mutator against a terminal state or an
// illegal (cur, target) edge. Map to HTTP 409.
var ErrInvalidTransition = errors.New("order: invalid state transition")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// Order is the aggregate root: one per-tenant row in orders.orders; the
// confirmed items snapshot lives in orders.order_items keyed by
// (tenant_id, order_id, line_number).
type Order struct {
	id                  ID
	tenantID            tenant.ID
	approvedQuotationID quotation.ID
	customerLeadID      quotation.CustomerLeadID
	state               State
	confirmedItems      []quotation.LineItem // frozen snapshot from quotation tip on approval
	subtotalPaise       int64
	taxPaise            int64
	grandTotalPaise     int64

	// Optional linkage IDs populated by downstream mutators.
	invoiceID         string // set when state >= invoiced
	consignmentNoteID string // set when state >= dispatched

	// Major-transition timestamps, surfaced on the row for read-side
	// dashboards without hitting the outbox.
	confirmedAt        *time.Time
	packedAt           *time.Time
	invoicedAt         *time.Time
	dispatchedAt       *time.Time
	deliveredAt        *time.Time
	completedAt        *time.Time
	cancelledAt        *time.Time
	cancellationReason string

	createdAt             time.Time
	createdByMembershipID membership.ID

	events []Event
}

// NewInput is the [New] constructor input, supplied by the
// ApproveQuotationCommand handler. ConfirmedItems is the caller's frozen
// revision items; totals are computed inside the ctor for invariant integrity.
type NewInput struct {
	ID                    ID
	TenantID              tenant.ID
	ApprovedQuotationID   quotation.ID
	CustomerLeadID        quotation.CustomerLeadID
	ConfirmedItems        []quotation.LineItem
	CreatedByMembershipID membership.ID
	Now                   time.Time
}

// New constructs an Order in state quotation_approved. Sole entry point — an
// Order cannot start in any other state; it exists iff a Quotation was approved.
func New(in NewInput) (*Order, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.ApprovedQuotationID.IsZero() {
		return nil, fmt.Errorf("%w: approved_quotation_id required", ErrInvalid)
	}
	if in.CustomerLeadID.IsZero() {
		return nil, fmt.Errorf("%w: customer_lead_id required", ErrInvalid)
	}
	if len(in.ConfirmedItems) == 0 {
		return nil, fmt.Errorf("%w: confirmed_items must be non-empty", ErrInvalid)
	}
	if in.CreatedByMembershipID == "" {
		return nil, fmt.Errorf("%w: created_by_membership_id required", ErrInvalid)
	}
	if in.Now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	for i := range in.ConfirmedItems {
		if err := in.ConfirmedItems[i].Validate(); err != nil {
			return nil, fmt.Errorf("%w: item[%d]: %w", ErrInvalid, i, err)
		}
	}

	itemsCopy := make([]quotation.LineItem, len(in.ConfirmedItems))
	copy(itemsCopy, in.ConfirmedItems)
	subtotal, tax, grand := computeTotals(itemsCopy)

	o := &Order{
		id:                    in.ID,
		tenantID:              in.TenantID,
		approvedQuotationID:   in.ApprovedQuotationID,
		customerLeadID:        in.CustomerLeadID,
		state:                 StateQuotationApproved,
		confirmedItems:        itemsCopy,
		subtotalPaise:         subtotal,
		taxPaise:              tax,
		grandTotalPaise:       grand,
		createdAt:             in.Now,
		createdByMembershipID: in.CreatedByMembershipID,
	}
	o.recordEvent(CreatedEvent{
		OrderID:               o.id,
		TenantID:              o.tenantID,
		ApprovedQuotationID:   o.approvedQuotationID,
		CustomerLeadID:        o.customerLeadID,
		GrandTotalPaise:       o.grandTotalPaise,
		CreatedAt:             o.createdAt,
		CreatedByMembershipID: o.createdByMembershipID,
	})
	return o, nil
}

// computeTotals sums items to (subtotal, tax, grand). Per ADR 0061:
//
//	line_subtotal = unit_sale_paise * quantity
//	line_tax      = line_subtotal * gst_rate_bps / 10000
//
// Integer-only — the bps representation carries no rounding risk.
func computeTotals(items []quotation.LineItem) (subtotal, tax, grand int64) {
	for _, it := range items {
		lineSubtotal := it.UnitSalePaise * int64(it.Quantity)
		lineTax := lineSubtotal * int64(it.GstRateBps) / 10000
		subtotal += lineSubtotal
		tax += lineTax
	}
	grand = subtotal + tax
	return
}

// Snapshot is the persistence DTO for [UnmarshalFromDB].
type Snapshot struct {
	ID                    ID
	TenantID              tenant.ID
	ApprovedQuotationID   quotation.ID
	CustomerLeadID        quotation.CustomerLeadID
	State                 State
	ConfirmedItems        []quotation.LineItem
	SubtotalPaise         int64
	TaxPaise              int64
	GrandTotalPaise       int64
	InvoiceID             string
	ConsignmentNoteID     string
	ConfirmedAt           *time.Time
	PackedAt              *time.Time
	InvoicedAt            *time.Time
	DispatchedAt          *time.Time
	DeliveredAt           *time.Time
	CompletedAt           *time.Time
	CancelledAt           *time.Time
	CancellationReason    string
	CreatedAt             time.Time
	CreatedByMembershipID membership.ID
}

// UnmarshalFromDB rehydrates the aggregate without re-validating. Adapter only.
func UnmarshalFromDB(s Snapshot) *Order {
	items := make([]quotation.LineItem, len(s.ConfirmedItems))
	copy(items, s.ConfirmedItems)
	return &Order{
		id:                    s.ID,
		tenantID:              s.TenantID,
		approvedQuotationID:   s.ApprovedQuotationID,
		customerLeadID:        s.CustomerLeadID,
		state:                 s.State,
		confirmedItems:        items,
		subtotalPaise:         s.SubtotalPaise,
		taxPaise:              s.TaxPaise,
		grandTotalPaise:       s.GrandTotalPaise,
		invoiceID:             s.InvoiceID,
		consignmentNoteID:     s.ConsignmentNoteID,
		confirmedAt:           s.ConfirmedAt,
		packedAt:              s.PackedAt,
		invoicedAt:            s.InvoicedAt,
		dispatchedAt:          s.DispatchedAt,
		deliveredAt:           s.DeliveredAt,
		completedAt:           s.CompletedAt,
		cancelledAt:           s.CancelledAt,
		cancellationReason:    s.CancellationReason,
		createdAt:             s.CreatedAt,
		createdByMembershipID: s.CreatedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (o *Order) ID() ID { return o.id }

// TenantID returns the owning tenant.
func (o *Order) TenantID() tenant.ID { return o.tenantID }

// ApprovedQuotationID returns the source quotation.
func (o *Order) ApprovedQuotationID() quotation.ID { return o.approvedQuotationID }

// CustomerLeadID returns the source CRM lead.
func (o *Order) CustomerLeadID() quotation.CustomerLeadID { return o.customerLeadID }

// State returns the current lifecycle state.
func (o *Order) State() State { return o.state }

// ConfirmedItems returns a defensive copy of the frozen items snapshot.
func (o *Order) ConfirmedItems() []quotation.LineItem {
	out := make([]quotation.LineItem, len(o.confirmedItems))
	copy(out, o.confirmedItems)
	return out
}

// SubtotalPaise returns the sum-of-line-subtotals.
func (o *Order) SubtotalPaise() int64 { return o.subtotalPaise }

// TaxPaise returns the sum-of-line-taxes.
func (o *Order) TaxPaise() int64 { return o.taxPaise }

// GrandTotalPaise returns subtotal + tax.
func (o *Order) GrandTotalPaise() int64 { return o.grandTotalPaise }

// InvoiceID returns the linked invoice ID once state >= invoiced;
// empty string otherwise.
func (o *Order) InvoiceID() string { return o.invoiceID }

// ConsignmentNoteID returns the linked dispatch ID once state >=
// dispatched; empty string otherwise.
func (o *Order) ConsignmentNoteID() string { return o.consignmentNoteID }

// CreatedAt returns the row-creation timestamp.
func (o *Order) CreatedAt() time.Time { return o.createdAt }

// CreatedByMembershipID returns the actor who created the order.
func (o *Order) CreatedByMembershipID() membership.ID { return o.createdByMembershipID }

// ConfirmedAt returns the confirmation timestamp.
func (o *Order) ConfirmedAt() *time.Time { return o.confirmedAt }

// PackedAt returns the packing timestamp.
func (o *Order) PackedAt() *time.Time { return o.packedAt }

// InvoicedAt returns the invoicing timestamp.
func (o *Order) InvoicedAt() *time.Time { return o.invoicedAt }

// DispatchedAt returns the dispatch timestamp.
func (o *Order) DispatchedAt() *time.Time { return o.dispatchedAt }

// DeliveredAt returns the delivery timestamp.
func (o *Order) DeliveredAt() *time.Time { return o.deliveredAt }

// CompletedAt returns the completion timestamp.
func (o *Order) CompletedAt() *time.Time { return o.completedAt }

// CancelledAt returns the cancellation timestamp.
func (o *Order) CancelledAt() *time.Time { return o.cancelledAt }

// CancellationReason returns the cancellation reason.
func (o *Order) CancellationReason() string { return o.cancellationReason }

// ----- State transitions ----------------------------------------------------

// RecordTokenPayment transitions quotation_approved → token_paid. Idempotent on self.
func (o *Order) RecordTokenPayment(actor membership.ID, now time.Time) error {
	return o.advance(StateTokenPaid, actor, now)
}

// Confirm transitions token_paid → confirmed, stamping confirmedAt. Idempotent
// on self. Downstream: subscribers to the emitted event reserve Inventory stock.
func (o *Order) Confirm(actor membership.ID, now time.Time) error {
	if err := o.advance(StateConfirmed, actor, now); err != nil {
		return err
	}
	if o.confirmedAt == nil {
		o.confirmedAt = &now
	}
	return nil
}

// MarkPacked transitions confirmed → packed.
func (o *Order) MarkPacked(actor membership.ID, now time.Time) error {
	if err := o.advance(StatePacked, actor, now); err != nil {
		return err
	}
	if o.packedAt == nil {
		o.packedAt = &now
	}
	return nil
}

// AttachInvoice transitions packed → invoiced and links invoiceID — the FK to
// the freshly-allocated invoice aggregate (which owns the gapless number).
func (o *Order) AttachInvoice(invoiceID string, actor membership.ID, now time.Time) error {
	if strings.TrimSpace(invoiceID) == "" {
		return fmt.Errorf("%w: invoice_id required", ErrInvalid)
	}
	if err := o.advance(StateInvoiced, actor, now); err != nil {
		return err
	}
	o.invoiceID = invoiceID
	if o.invoicedAt == nil {
		o.invoicedAt = &now
	}
	return nil
}

// AttachConsignment transitions invoiced → dispatched and links the
// consignment-note ID. The note lives in the Dispatch schema; Order stores the
// FK as text (cross-schema reference).
//
// Replay-tolerant (at-least-once saga delivery): re-attaching the SAME
// consignment is a no-op regardless of how far the order has advanced since
// (delivered/complete included). Attaching a DIFFERENT consignment when one is
// already linked is ErrInvalid — Dispatch enforces one note per order, so a
// second ID is a producer bug, never a legitimate replacement.
func (o *Order) AttachConsignment(consignmentID string, actor membership.ID, now time.Time) error {
	consignmentID = strings.TrimSpace(consignmentID)
	if consignmentID == "" {
		return fmt.Errorf("%w: consignment_id required", ErrInvalid)
	}
	if o.consignmentNoteID == consignmentID {
		return nil // replay — already attached
	}
	if o.consignmentNoteID != "" {
		return fmt.Errorf("%w: consignment %s already attached", ErrInvalid, o.consignmentNoteID)
	}
	if err := o.advance(StateDispatched, actor, now); err != nil {
		return err
	}
	o.consignmentNoteID = consignmentID
	if o.dispatchedAt == nil {
		o.dispatchedAt = &now
	}
	return nil
}

// MarkDelivered transitions dispatched → delivered. Driven by the Dispatch
// subscriber on carrier delivery confirmation.
//
// Replay-tolerant: once deliveredAt is stamped, later replays are no-ops even
// after the order completes (terminal) — the saga's at-least-once redelivery
// must never error on an already-delivered order.
func (o *Order) MarkDelivered(actor membership.ID, now time.Time) error {
	if o.deliveredAt != nil {
		return nil // replay — already delivered (possibly already complete)
	}
	if err := o.advance(StateDelivered, actor, now); err != nil {
		return err
	}
	o.deliveredAt = &now
	return nil
}

// MarkComplete transitions delivered → complete. The calling command handler
// gates this on the FullPayment aggregate confirming the balance is paid.
func (o *Order) MarkComplete(actor membership.ID, now time.Time) error {
	if err := o.advance(StateComplete, actor, now); err != nil {
		return err
	}
	if o.completedAt == nil {
		o.completedAt = &now
	}
	return nil
}

// Cancel transitions any non-terminal state → cancelled with reason. The
// cancel-time state decides which compensation subscribers fire (ADR 0063 §4).
// Idempotent on self.
func (o *Order) Cancel(reason string, actor membership.ID, now time.Time) error {
	if o.state == StateCancelled {
		return nil
	}
	if o.state == StateComplete {
		return fmt.Errorf("%w: cannot cancel completed order", ErrInvalidTransition)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: cancellation reason required", ErrInvalid)
	}
	if actor == "" {
		return fmt.Errorf("%w: actor required", ErrInvalid)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	priorState := o.state
	o.state = StateCancelled
	o.cancellationReason = reason
	o.cancelledAt = &now
	o.recordEvent(CancelledEvent{
		OrderID:               o.id,
		TenantID:              o.tenantID,
		PriorState:            priorState,
		Reason:                reason,
		CancelledAt:           now,
		CancelledByMembership: actor,
	})
	return nil
}

// advance is the shared transition primitive: validate (self-idempotent,
// terminal-guard, canAdvance), set state, emit AdvancedEvent.
func (o *Order) advance(target State, actor membership.ID, now time.Time) error {
	if o.state == target {
		return nil // self-transition: idempotent no-op
	}
	if o.state.IsTerminal() {
		return fmt.Errorf("%w: cannot transition from terminal state %s", ErrInvalidTransition, o.state)
	}
	if !canAdvance(o.state, target) {
		return fmt.Errorf("%w: %s → %s not permitted", ErrInvalidTransition, o.state, target)
	}
	if actor == "" {
		return fmt.Errorf("%w: actor required", ErrInvalid)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	priorState := o.state
	o.state = target
	o.recordEvent(AdvancedEvent{
		OrderID:                  o.id,
		TenantID:                 o.tenantID,
		PriorState:               priorState,
		NewState:                 target,
		TransitionedAt:           now,
		TransitionedByMembership: actor,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains and returns the recorded domain events.
func (o *Order) PullEvents() []Event {
	if len(o.events) == 0 {
		return nil
	}
	out := o.events
	o.events = nil
	return out
}

func (o *Order) recordEvent(e Event) { o.events = append(o.events, e) }
