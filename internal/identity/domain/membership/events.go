package membership

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the marker interface for Membership domain events.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// CreatedEvent fires on [New].
type CreatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (CreatedEvent) Topic() string { return "identity.membership_created.v1" }

// OccurredAt returns the domain timestamp.
func (e CreatedEvent) OccurredAt() time.Time { return e.At }

// DeactivatedEvent fires when a Membership transitions Active → Inactive.
type DeactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	Reason       string
	At           time.Time
}

// Topic returns the integration-event type.
func (DeactivatedEvent) Topic() string { return "identity.membership_deactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e DeactivatedEvent) OccurredAt() time.Time { return e.At }

// ReactivatedEvent fires when a Membership transitions Inactive → Active.
type ReactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (ReactivatedEvent) Topic() string { return "identity.membership_reactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e ReactivatedEvent) OccurredAt() time.Time { return e.At }
