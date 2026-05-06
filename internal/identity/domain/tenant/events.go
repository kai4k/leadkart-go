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

// ProfileUpdatedEvent fires when [Tenant.UpdateProfile] is called.
//
// Carries OLD + NEW values so audit + downstream subscribers can render
// the diff without re-loading the aggregate. Mirrors the .NET parent's
// TenantProfileUpdated integration event.
type ProfileUpdatedEvent struct {
	TenantID       ID
	OldLegalName   string
	OldDisplayName string
	NewLegalName   string
	NewDisplayName string
	At             time.Time
}

// Topic returns the integration event type.
func (ProfileUpdatedEvent) Topic() string { return "identity.tenant_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e ProfileUpdatedEvent) OccurredAt() time.Time { return e.At }

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

// MarkedForDeletionEvent fires when [Tenant.MarkForDeletion] is called.
//
// Per data-retention.md "Tenant deletion saga": entry into the 30-day
// grace window. Subscribers (CRM, Orders, etc.) MAY block tenant ops
// immediately or wait for the terminal DeletedEvent — implementation
// choice per module.
type MarkedForDeletionEvent struct {
	TenantID    ID
	Reason      string
	ScheduledAt time.Time
	At          time.Time
}

// Topic returns the integration event type.
func (MarkedForDeletionEvent) Topic() string { return "identity.tenant_marked_for_deletion.v1" }

// OccurredAt returns the domain timestamp.
func (e MarkedForDeletionEvent) OccurredAt() time.Time { return e.At }

// RestoredEvent fires when [Tenant.RestoreFromDeletion] cancels a
// pending deletion within the grace window.
//
// Subscribers that blocked ops on MarkedForDeletionEvent re-enable.
type RestoredEvent struct {
	TenantID ID
	At       time.Time
}

// Topic returns the integration event type.
func (RestoredEvent) Topic() string { return "identity.tenant_restored.v1" }

// OccurredAt returns the domain timestamp.
func (e RestoredEvent) OccurredAt() time.Time { return e.At }

// DeletedEvent fires when [Tenant.HardDelete] is called by the
// data-retention saga after the grace window expires.
//
// Per data-retention.md: terminal state. Subscribers SHOULD anonymise
// remaining PII per their module's classification (CRM lead notes,
// Tasks comments). Audit log retained 7 years; tenant row retained
// for FK integrity.
type DeletedEvent struct {
	TenantID ID
	Reason   string
	At       time.Time
}

// Topic returns the integration event type.
func (DeletedEvent) Topic() string { return "identity.tenant_deleted.v1" }

// OccurredAt returns the domain timestamp.
func (e DeletedEvent) OccurredAt() time.Time { return e.At }
