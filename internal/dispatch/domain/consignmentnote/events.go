package consignmentnote

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isConsignmentNoteEvent() }

// CreatedEvent fires on ctor (a pending ConsignmentNote slot is born).
// Typically driven by the OrderPacked subscriber.
type CreatedEvent struct {
	ConsignmentNoteID     ID
	TenantID              tenant.ID
	OrderID               OrderID
	CarrierName           string
	BoxCount              int32
	WeightGrams           int64
	ExpectedDeliveryAt    *time.Time
	CreatedAt             time.Time
	CreatedByMembershipID membership.ID
}

func (CreatedEvent) isConsignmentNoteEvent() {}

// StatusChangedEvent fires on every advance + on terminal transitions.
// The (PriorStatus, NewStatus) pair lets subscribers route on either
// side without losing context — in particular, the Orders subscriber
// filters NewStatus=delivered to drive its own state transition.
type StatusChangedEvent struct {
	ConsignmentNoteID        ID
	TenantID                 tenant.ID
	OrderID                  OrderID
	PriorStatus              Status
	NewStatus                Status
	TransitionedAt           time.Time
	TransitionedByMembership membership.ID
}

func (StatusChangedEvent) isConsignmentNoteEvent() {}
