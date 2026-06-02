package order

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isOrderEvent() }

// CreatedEvent fires on ctor — an Order starts in quotation_approved when its
// source Quotation is approved.
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

// AdvancedEvent fires on every forward transition. PriorState + NewState carry
// the diff so subscribers can route on either side.
//
// Per ADR 0063 subscribers route on NewState — Inventory stock-reservation
// filters NewState=confirmed; Notifications "order shipped" filters
// NewState=dispatched.
type AdvancedEvent struct {
	OrderID                  ID
	TenantID                 tenant.ID
	PriorState               State
	NewState                 State
	TransitionedAt           time.Time
	TransitionedByMembership membership.ID
}

func (AdvancedEvent) isOrderEvent() {}

// CancelledEvent fires when Cancel moves the order to the terminal cancelled
// state. PriorState (the state at cancel-time) drives compensation: invoiced
// fires the Invoice → CreditNote subscriber; dispatched also fires the
// Dispatch → cancel-consignment subscriber.
type CancelledEvent struct {
	OrderID               ID
	TenantID              tenant.ID
	PriorState            State
	Reason                string
	CancelledAt           time.Time
	CancelledByMembership membership.ID
}

func (CancelledEvent) isOrderEvent() {}
