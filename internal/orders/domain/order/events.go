package order

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isOrderEvent() }

// CreatedEvent fires on ctor (Order is created in state
// quotation_approved when the source Quotation is approved).
type CreatedEvent struct {
	OrderID               ID
	TenantID              tenant.ID
	ApprovedQuotationID   quotation.ID
	CustomerLeadID        quotation.CustomerLeadID
	GrandTotalPaise       int64
	CreatedAt             time.Time
	CreatedByMembershipID membership.ID
}

func (CreatedEvent) isOrderEvent() {}

// AdvancedEvent fires on every forward state transition (token paid,
// confirmed, packed, invoiced, dispatched, delivered, complete).
// PriorState + NewState carry the diff so subscribers can route on
// either side without losing context.
//
// Per ADR 0063: subscribers route on NewState — e.g. the Inventory
// stock-reservation subscriber filters for NewState=confirmed; the
// Notifications "your order shipped" subscriber filters for
// NewState=dispatched.
type AdvancedEvent struct {
	OrderID                 ID
	TenantID                tenant.ID
	PriorState              State
	NewState                State
	TransitionedAt          time.Time
	TransitionedByMembership membership.ID
}

func (AdvancedEvent) isOrderEvent() {}

// CancelledEvent fires when Cancel transitions the order to the
// terminal `cancelled` state. PriorState is the state at cancel-time
// — the compensation-driving fact (e.g. PriorState=invoiced means
// the Invoice → CreditNote subscriber should fire; PriorState=
// dispatched means the Dispatch → cancel-consignment subscriber also
// fires).
type CancelledEvent struct {
	OrderID               ID
	TenantID              tenant.ID
	PriorState            State
	Reason                string
	CancelledAt           time.Time
	CancelledByMembership membership.ID
}

func (CancelledEvent) isOrderEvent() {}
