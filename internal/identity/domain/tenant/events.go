package tenant

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// Event is the sealed marker interface for tenant domain events. Sealed via
// the unexported isTenantEvent() method — only types in this package satisfy it.
//
// Events carry no wire concerns (topic, envelope, version alias). Wire
// versioning lives in integrationevents.*V1 per Vernon IDDD ch. 8; a wire
// rename must not force a domain edit. The integration mapper
// type-switches on these structs to emit the V1 envelope.
//
// Events are recorded by aggregate methods and drained by the repository
// inside the same transaction as the state mutation (ADR 0004 + ADR 0008).
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
// Carries old and new values so subscribers can render diffs without
// re-loading the aggregate.
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

// StatutoryUpdatedEvent fires when [Tenant.UpdateStatutory] changes any
// declared Indian statutory ID (GST/PAN/DrugLicence). Zero OldStatutory
// indicates first declaration.
type StatutoryUpdatedEvent struct {
	TenantID     ID
	OldStatutory Statutory
	NewStatutory Statutory
	At           time.Time
}

func (StatutoryUpdatedEvent) isTenantEvent() {}

// AdminContactUpdatedEvent fires when [Tenant.UpdateAdminContact] changes
// the admin phone or postal address. Zero OldAdminContact indicates first
// declaration.
type AdminContactUpdatedEvent struct {
	TenantID        ID
	OldAdminContact AdminContact
	NewAdminContact AdminContact
	At              time.Time
}

func (AdminContactUpdatedEvent) isTenantEvent() {}

// SettingsUpdatedEvent fires when [Tenant.UpdateSettings] changes operational
// settings. Auth and login-flow caches must consume this to invalidate
// cached policy.
type SettingsUpdatedEvent struct {
	TenantID    ID
	OldSettings Settings
	NewSettings Settings
	At          time.Time
}

func (SettingsUpdatedEvent) isTenantEvent() {}

// DisplayPreferencesUpdatedEvent fires when [Tenant.UpdateDisplayPreferences]
// changes UI rendering preferences. Subscribers (BFF cache, notification
// renderers) must invalidate cached preferences on receipt.
type DisplayPreferencesUpdatedEvent struct {
	TenantID              ID
	OldDisplayPreferences DisplayPreferences
	NewDisplayPreferences DisplayPreferences
	At                    time.Time
}

func (DisplayPreferencesUpdatedEvent) isTenantEvent() {}

// MarkedForDeletionEvent fires when [Tenant.MarkForDeletion] is called,
// entering the 30-day grace window (data-retention.md). Subscribers may
// block tenant ops immediately or wait for DeletedEvent — per-module choice.
type MarkedForDeletionEvent struct {
	TenantID    ID
	Reason      string
	ScheduledAt time.Time
	At          time.Time
}

func (MarkedForDeletionEvent) isTenantEvent() {}

// RestoredEvent fires when [Tenant.RestoreFromDeletion] cancels a pending
// deletion. Subscribers that blocked ops on MarkedForDeletionEvent must re-enable.
type RestoredEvent struct {
	TenantID ID
	At       time.Time
}

func (RestoredEvent) isTenantEvent() {}

// DeletedEvent fires when [Tenant.HardDelete] is called after the grace
// window expires. Terminal state. Subscribers should anonymise remaining PII
// per their module's data-retention classification. Tenant row is retained
// for FK integrity; audit log is kept 7 years.
type DeletedEvent struct {
	TenantID ID
	Reason   string
	At       time.Time
}

func (DeletedEvent) isTenantEvent() {}
