// Package quotation owns the [Quotation] aggregate — the proposal /
// draft document that precedes an [order.Order]. Per BRD §4.8 the
// informal name is "Kacha Bill"; quotation is the correct term.
//
// State machine (strict; no skips, no backtracking):
//
//	draft → revised* → approved (terminal-success)
//	       ↘ rejected (terminal-failure)
//
// Quotation owns its own revision chain — every Revise call appends a
// new revision tuple (items snapshot, note, revised-by, revised-at) to
// the [Quotation.Revisions] slice. The TIP of that slice is the
// "current" quotation; earlier entries are immutable history. Approval
// freezes the tip; Order then COPIES the tip into its own
// `confirmed_items` snapshot so the Order's state is independent of
// any later Quotation lifecycle.
//
// Per ADR 0063: separate aggregate from Order, not a child entity.
// Quotation revisions are their own audit need; collapsing into Order
// would force a JSON revision-history column that breaks the
// "Order owns confirmed-state, Quotation owns proposal-state"
// invariant separation.
package quotation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel for invariant violations (ctor + mutator).
// Map to HTTP 422 at the port boundary.
var ErrInvalid = errors.New("quotation: invalid")

// ErrInvalidTransition is returned when a mutator is called against a
// terminal state (approved / rejected) or against a non-precondition
// state. Map to HTTP 409 Conflict.
var ErrInvalidTransition = errors.New("quotation: invalid state transition")

// ID is a UUIDv7 minted at command-handler time.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// CustomerLeadID references the source `crm.crm_leads` row this
// quotation was raised against. Stored as a string to keep the
// boundary clean (no Orders → CRM domain import).
type CustomerLeadID string

// IsZero reports whether c is unset.
func (c CustomerLeadID) IsZero() bool { return c == "" }

// String returns the underlying UUID string.
func (c CustomerLeadID) String() string { return string(c) }

// LineItem is a single product row on a quotation revision. Prices are
// `int64 paise` per ADR 0061 (Stripe canon). Quantity is an integer
// number of UNITS (pack-aware semantics live in the application layer).
type LineItem struct {
	ProductID    string
	SKU          string
	Description  string
	Quantity     int32
	UnitMrpPaise int64
	UnitSalePaise int64
	GstRateBps   int32 // basis points (12% = 1200) per inventory canon
}

// Validate runs the per-item invariants. Used by ctor + mutators.
func (li LineItem) Validate() error {
	if li.ProductID == "" {
		return fmt.Errorf("%w: line.product_id required", ErrInvalid)
	}
	if li.SKU == "" {
		return fmt.Errorf("%w: line.sku required", ErrInvalid)
	}
	if li.Quantity <= 0 {
		return fmt.Errorf("%w: line.quantity must be positive (got %d)", ErrInvalid, li.Quantity)
	}
	if li.UnitSalePaise <= 0 {
		return fmt.Errorf("%w: line.unit_sale_paise must be positive", ErrInvalid)
	}
	if li.UnitMrpPaise > 0 && li.UnitSalePaise > li.UnitMrpPaise {
		// Sale > MRP is a regulatory violation under DPCO; reject at
		// the boundary so the front-end shows a clear error rather
		// than letting it ride through Approval.
		return fmt.Errorf("%w: line.unit_sale_paise (%d) exceeds line.unit_mrp_paise (%d)",
			ErrInvalid, li.UnitSalePaise, li.UnitMrpPaise)
	}
	if li.GstRateBps < 0 || li.GstRateBps > 10000 {
		return fmt.Errorf("%w: line.gst_rate_bps must be 0..10000 (got %d)", ErrInvalid, li.GstRateBps)
	}
	return nil
}

// Revision is one tuple in the quotation's history chain.
type Revision struct {
	Number          int64 // 1-indexed; tip is len(Revisions)
	Items           []LineItem
	Note            string
	RevisedAt       time.Time
	RevisedByMembership membership.ID
}

// State is the lifecycle position of the Quotation aggregate.
type State string

// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on orders.quotations.state in the init migration.
const (
	StateDraft    State = "draft"
	StateApproved State = "approved"
	StateRejected State = "rejected"
)

// String returns the wire form.
func (s State) String() string { return string(s) }

// IsTerminal reports whether the state allows NO further transitions.
func (s State) IsTerminal() bool { return s == StateApproved || s == StateRejected }

// IsValid reports whether s is a known catalogue entry.
func (s State) IsValid() bool {
	switch s {
	case StateDraft, StateApproved, StateRejected:
		return true
	}
	return false
}

// ParseState turns an untrusted string into a [State] or returns
// [ErrInvalid] wrapped with the bad value.
func ParseState(raw string) (State, error) {
	s := State(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: state %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// Quotation is the aggregate root. One per-tenant row in
// `orders.quotations`; revisions live in `orders.quotation_revisions`
// keyed by (tenant_id, quotation_id, number).
type Quotation struct {
	id              ID
	tenantID        tenant.ID
	customerLeadID  CustomerLeadID
	state           State
	revisions       []Revision
	approvedAt      *time.Time
	approvedByMembershipID *membership.ID
	rejectedAt      *time.Time
	rejectedByMembershipID *membership.ID
	rejectionReason string
	createdAt       time.Time
	createdByMembershipID membership.ID

	events []Event
}

// NewInput is the constructor input for a fresh draft quotation.
type NewInput struct {
	ID                    ID
	TenantID              tenant.ID
	CustomerLeadID        CustomerLeadID
	InitialItems          []LineItem
	InitialNote           string
	CreatedByMembershipID membership.ID
	Now                   time.Time
}

// New constructs a draft Quotation with the seed (revision 1). Every
// invariant is enforced at the boundary — once New returns nil error,
// the aggregate is valid by construction.
func New(in NewInput) (*Quotation, error) {
	if in.ID.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if in.TenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if in.CustomerLeadID.IsZero() {
		return nil, fmt.Errorf("%w: customer_lead_id required", ErrInvalid)
	}
	if in.CreatedByMembershipID == "" {
		return nil, fmt.Errorf("%w: created_by_membership_id required", ErrInvalid)
	}
	if in.Now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	if len(in.InitialItems) == 0 {
		return nil, fmt.Errorf("%w: initial_items must be non-empty", ErrInvalid)
	}
	for i := range in.InitialItems {
		if err := in.InitialItems[i].Validate(); err != nil {
			return nil, fmt.Errorf("%w: item[%d]: %w", ErrInvalid, i, err)
		}
	}

	itemsCopy := make([]LineItem, len(in.InitialItems))
	copy(itemsCopy, in.InitialItems)

	q := &Quotation{
		id:                    in.ID,
		tenantID:              in.TenantID,
		customerLeadID:        in.CustomerLeadID,
		state:                 StateDraft,
		revisions:             []Revision{{
			Number:              1,
			Items:               itemsCopy,
			Note:                strings.TrimSpace(in.InitialNote),
			RevisedAt:           in.Now,
			RevisedByMembership: in.CreatedByMembershipID,
		}},
		createdAt:             in.Now,
		createdByMembershipID: in.CreatedByMembershipID,
	}
	q.recordEvent(CreatedEvent{
		QuotationID:           q.id,
		TenantID:              q.tenantID,
		CustomerLeadID:        q.customerLeadID,
		LineItemCount:         int64(len(itemsCopy)),
		CreatedAt:             q.createdAt,
		CreatedByMembershipID: q.createdByMembershipID,
	})
	return q, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                     ID
	TenantID               tenant.ID
	CustomerLeadID         CustomerLeadID
	State                  State
	Revisions              []Revision
	ApprovedAt             *time.Time
	ApprovedByMembershipID *membership.ID
	RejectedAt             *time.Time
	RejectedByMembershipID *membership.ID
	RejectionReason        string
	CreatedAt              time.Time
	CreatedByMembershipID  membership.ID
}

// UnmarshalFromDB rehydrates the aggregate without re-validating —
// the DB schema is the source of truth at this point.
func UnmarshalFromDB(s Snapshot) *Quotation {
	revs := make([]Revision, len(s.Revisions))
	copy(revs, s.Revisions)
	return &Quotation{
		id:                     s.ID,
		tenantID:               s.TenantID,
		customerLeadID:         s.CustomerLeadID,
		state:                  s.State,
		revisions:              revs,
		approvedAt:             s.ApprovedAt,
		approvedByMembershipID: s.ApprovedByMembershipID,
		rejectedAt:             s.RejectedAt,
		rejectedByMembershipID: s.RejectedByMembershipID,
		rejectionReason:        s.RejectionReason,
		createdAt:              s.CreatedAt,
		createdByMembershipID:  s.CreatedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (q *Quotation) ID() ID { return q.id }

// TenantID returns the owning tenant.
func (q *Quotation) TenantID() tenant.ID { return q.tenantID }

// CustomerLeadID returns the source CRM lead.
func (q *Quotation) CustomerLeadID() CustomerLeadID { return q.customerLeadID }

// State returns the current lifecycle state.
func (q *Quotation) State() State { return q.state }

// Revisions returns a defensive copy of the revision chain. The tip is
// the LAST element.
func (q *Quotation) Revisions() []Revision {
	out := make([]Revision, len(q.revisions))
	copy(out, q.revisions)
	return out
}

// CurrentRevision returns the tip revision (the latest set of items).
// Quotation construction guarantees at least one revision exists.
func (q *Quotation) CurrentRevision() Revision { return q.revisions[len(q.revisions)-1] }

// ApprovedAt returns the approval timestamp + actor, or nil pointer
// when not approved.
func (q *Quotation) ApprovedAt() *time.Time            { return q.approvedAt }

// ApprovedByMembershipID returns the actor who approved or nil pointer.
func (q *Quotation) ApprovedByMembershipID() *membership.ID { return q.approvedByMembershipID }

// RejectedAt returns the rejection timestamp + actor + reason, or
// nil pointer when not rejected.
func (q *Quotation) RejectedAt() *time.Time            { return q.rejectedAt }

// RejectedByMembershipID returns the actor who rejected.
func (q *Quotation) RejectedByMembershipID() *membership.ID { return q.rejectedByMembershipID }

// RejectionReason returns the operator-supplied reason on terminal-fail.
func (q *Quotation) RejectionReason() string           { return q.rejectionReason }

// CreatedAt returns the row-creation timestamp.
func (q *Quotation) CreatedAt() time.Time              { return q.createdAt }

// CreatedByMembershipID returns the actor who raised the quote.
func (q *Quotation) CreatedByMembershipID() membership.ID { return q.createdByMembershipID }

// ----- State transitions ----------------------------------------------------

// ReviseInput is the input shape for [Quotation.Revise]. Note is
// optional but recommended — appears on the audit trail.
type ReviseInput struct {
	Items               []LineItem
	Note                string
	RevisedByMembership membership.ID
	Now                 time.Time
}

// Revise appends a new revision tuple with the supplied items snapshot.
// Rejected against terminal states; the items must be non-empty + each
// item must pass [LineItem.Validate].
func (q *Quotation) Revise(in ReviseInput) error {
	if q.state.IsTerminal() {
		return fmt.Errorf("%w: cannot revise quotation in state %s", ErrInvalidTransition, q.state)
	}
	if len(in.Items) == 0 {
		return fmt.Errorf("%w: revision items must be non-empty", ErrInvalid)
	}
	for i := range in.Items {
		if err := in.Items[i].Validate(); err != nil {
			return fmt.Errorf("%w: item[%d]: %w", ErrInvalid, i, err)
		}
	}
	if in.RevisedByMembership == "" {
		return fmt.Errorf("%w: revised_by required", ErrInvalid)
	}
	if in.Now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}

	itemsCopy := make([]LineItem, len(in.Items))
	copy(itemsCopy, in.Items)
	rev := Revision{
		Number:              int64(len(q.revisions)) + 1,
		Items:               itemsCopy,
		Note:                strings.TrimSpace(in.Note),
		RevisedAt:           in.Now,
		RevisedByMembership: in.RevisedByMembership,
	}
	q.revisions = append(q.revisions, rev)
	q.recordEvent(RevisedEvent{
		QuotationID:         q.id,
		TenantID:            q.tenantID,
		RevisionNumber:      rev.Number,
		LineItemCount:       int64(len(itemsCopy)),
		Note:                rev.Note,
		RevisedAt:           rev.RevisedAt,
		RevisedByMembership: rev.RevisedByMembership,
	})
	return nil
}

// Approve flips state to approved, freezing the tip revision as the
// authoritative items snapshot. Idempotent on self (already-approved
// returns nil + emits no event). Rejected against terminal-fail
// (rejected) state.
func (q *Quotation) Approve(approverID membership.ID, now time.Time) error {
	if q.state == StateApproved {
		return nil // idempotent
	}
	if q.state == StateRejected {
		return fmt.Errorf("%w: cannot approve rejected quotation", ErrInvalidTransition)
	}
	if approverID == "" {
		return fmt.Errorf("%w: approver required", ErrInvalid)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	q.state = StateApproved
	q.approvedAt = &now
	q.approvedByMembershipID = &approverID
	q.recordEvent(ApprovedEvent{
		QuotationID:            q.id,
		TenantID:               q.tenantID,
		CustomerLeadID:         q.customerLeadID,
		ApprovedRevisionNumber: q.CurrentRevision().Number,
		ApprovedAt:             now,
		ApprovedByMembership:   approverID,
	})
	return nil
}

// Reject flips state to rejected with the supplied reason. Idempotent
// on self. Rejected against terminal-success (approved) state.
func (q *Quotation) Reject(rejectorID membership.ID, reason string, now time.Time) error {
	if q.state == StateRejected {
		return nil // idempotent
	}
	if q.state == StateApproved {
		return fmt.Errorf("%w: cannot reject approved quotation", ErrInvalidTransition)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: rejection reason required", ErrInvalid)
	}
	if rejectorID == "" {
		return fmt.Errorf("%w: rejector required", ErrInvalid)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	q.state = StateRejected
	q.rejectedAt = &now
	q.rejectedByMembershipID = &rejectorID
	q.rejectionReason = reason
	q.recordEvent(RejectedEvent{
		QuotationID:           q.id,
		TenantID:              q.tenantID,
		Reason:                reason,
		RejectedAt:            now,
		RejectedByMembership:  rejectorID,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains + returns the recorded domain events. The repo
// calls this inside the persist tx so events are saved atomically with
// the state change.
func (q *Quotation) PullEvents() []Event {
	if len(q.events) == 0 {
		return nil
	}
	out := q.events
	q.events = nil
	return out
}

func (q *Quotation) recordEvent(e Event) { q.events = append(q.events, e) }
