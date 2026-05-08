package tenant

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// Event is the SEALED marker interface for tenant domain events.
// Sealed via the unexported isTenantEvent() method so only types in
// this package can satisfy it — same shape as role.Event.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// integrationevents.*V1 per Vernon IDDD ch. 8 ("Domain Events vs.
// Integration Events"): a v2 wire rename must NOT force a domain edit.
// The integration mapper in internal/identity/integrationevents/
// type-switches on these structs and emits the canonical V1 envelope.
//
// Domain events are recorded by aggregate methods and drained by
// repositories on persist. The repository's UpdateByID closure pattern
// (per ADR 0004 + TDL Sep 2024) writes events to the outbox table in
// the same transaction as the state mutation.
type Event interface {
	isTenantEvent()
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

func (RegisteredEvent) isTenantEvent() {}

// ActivatedEvent fires when a Tenant transitions to [StatusActive].
type ActivatedEvent struct {
	TenantID ID
	At       time.Time
}

func (ActivatedEvent) isTenantEvent() {}

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

func (ProfileUpdatedEvent) isTenantEvent() {}

// SuspendedEvent fires when a Tenant transitions to [StatusSuspended].
type SuspendedEvent struct {
	TenantID ID
	Reason   string
	At       time.Time
}

func (SuspendedEvent) isTenantEvent() {}

// StatutoryUpdatedEvent fires when [Tenant.UpdateStatutory] changes
// any of the declared Indian statutory IDs (GST/PAN/DrugLicence).
//
// Carries the OLD and NEW Statutory values so audit subscribers can
// render diffs. Empty (zero) Statutory in OldStatutory means the
// tenant declared statutory IDs for the first time.
type StatutoryUpdatedEvent struct {
	TenantID     ID
	OldStatutory Statutory
	NewStatutory Statutory
	At           time.Time
}

func (StatutoryUpdatedEvent) isTenantEvent() {}

// AdminContactUpdatedEvent fires when [Tenant.UpdateAdminContact]
// changes the admin phone or postal address. Carries OLD/NEW for
// audit-diff rendering. Empty (zero) AdminContact in OldAdminContact
// means the tenant declared contact details for the first time.
type AdminContactUpdatedEvent struct {
	TenantID        ID
	OldAdminContact AdminContact
	NewAdminContact AdminContact
	At              time.Time
}

func (AdminContactUpdatedEvent) isTenantEvent() {}

// SettingsUpdatedEvent fires when [Tenant.UpdateSettings] changes
// the tenant's operational settings (password policy today).
//
// Auth + login-flow caches MUST consume this to invalidate cached
// policy — incorrect cached policy means stale rules until cache TTL.
type SettingsUpdatedEvent struct {
	TenantID    ID
	OldSettings Settings
	NewSettings Settings
	At          time.Time
}

func (SettingsUpdatedEvent) isTenantEvent() {}

// DisplayPreferencesUpdatedEvent fires when
// [Tenant.UpdateDisplayPreferences] changes the tenant's UI rendering
// preferences. Subscribers (web BFF preference cache, notification
// renderers) consume to invalidate cached preferences.
type DisplayPreferencesUpdatedEvent struct {
	TenantID              ID
	OldDisplayPreferences DisplayPreferences
	NewDisplayPreferences DisplayPreferences
	At                    time.Time
}

func (DisplayPreferencesUpdatedEvent) isTenantEvent() {}

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

func (MarkedForDeletionEvent) isTenantEvent() {}

// RestoredEvent fires when [Tenant.RestoreFromDeletion] cancels a
// pending deletion within the grace window.
//
// Subscribers that blocked ops on MarkedForDeletionEvent re-enable.
type RestoredEvent struct {
	TenantID ID
	At       time.Time
}

func (RestoredEvent) isTenantEvent() {}

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

func (DeletedEvent) isTenantEvent() {}
