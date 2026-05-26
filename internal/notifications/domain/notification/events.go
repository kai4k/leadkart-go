package notification

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isNotificationEvent() }

// CreatedEvent fires on ctor. The real-time-push side wires off this
// event (subscriber writes to coder/websocket per ADR 0016).
type CreatedEvent struct {
	NotificationID        ID
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	Category              Category
	SourceModule          string
	SourceEntityType      string
	SourceEntityID        string
	CreatedAt             time.Time
}

func (CreatedEvent) isNotificationEvent() {}

// MarkedReadEvent fires on MarkRead. Drives badge-count refresh on
// the recipient's open connections.
type MarkedReadEvent struct {
	NotificationID        ID
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	ReadAt                time.Time
}

func (MarkedReadEvent) isNotificationEvent() {}

// DismissedEvent fires on Dismiss. PriorState records whether the
// recipient dismissed before or after reading.
type DismissedEvent struct {
	NotificationID        ID
	TenantID              tenant.ID
	RecipientMembershipID membership.ID
	PriorState            State
	DismissedAt           time.Time
}

func (DismissedEvent) isNotificationEvent() {}
