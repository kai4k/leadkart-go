package tenant

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// Event is the marker interface for tenant domain events.
//
// Domain events are recorded by aggregate methods and drained by
// repositories on persist. The repository's UpdateByID closure pattern
// (per ADR 0004 + TDL Sep 2024) writes events to the outbox table in
// the same transaction as the state mutation.
//
// Each event carries `Topic()` (event_type metadata) and `OccurredAt()`
// (domain-time, distinct from row creation time).
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// RegisteredEvent fires when a Tenant is created via [New].
type RegisteredEvent struct {
	TenantID    ID
	Slug        slug.Slug
	LegalName   string
	DisplayName string
	AdminEmail  email.Address
	At          time.Time
}

// Topic returns the integration event type. Versioned per `messaging.md`
// canon — bump to .v2 on any breaking field change.
func (RegisteredEvent) Topic() string { return "identity.tenant_registered.v1" }

// OccurredAt returns the domain timestamp when registration happened.
func (e RegisteredEvent) OccurredAt() time.Time { return e.At }

// ActivatedEvent fires when a Tenant transitions to [StatusActive].
type ActivatedEvent struct {
	TenantID ID
	At       time.Time
}

// Topic returns the integration event type.
func (ActivatedEvent) Topic() string { return "identity.tenant_activated.v1" }

// OccurredAt returns the domain timestamp.
func (e ActivatedEvent) OccurredAt() time.Time { return e.At }

// SuspendedEvent fires when a Tenant transitions to [StatusSuspended].
type SuspendedEvent struct {
	TenantID ID
	Reason   string
	At       time.Time
}

// Topic returns the integration event type.
func (SuspendedEvent) Topic() string { return "identity.tenant_suspended.v1" }

// OccurredAt returns the domain timestamp.
func (e SuspendedEvent) OccurredAt() time.Time { return e.At }
