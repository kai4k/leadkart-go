// Package tenant defines the Tenant aggregate (ADR 0002, multi-tenancy.md).
//
// Tenant is not itself tenant-scoped — each row IS a tenant. Identified by
// [ID] (UUIDv7) and addressed by [Slug]. Construction is via [New] (invariant
// enforcement) or [UnmarshalFromDB] (repository re-hydration, no re-validation).
package tenant

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// ErrInvalid is returned (wrapped via %w) on invariant violation.
// Callers branch via [errors.Is].
var ErrInvalid = errs.New(errs.KindInvalidInput, "tenant", "invalid tenant")

// PlatformSlug is the canonical slug of the LeadKart platform tenant.
// Both the login command and platform-tier middleware anchor on this constant
// so they can never disagree about which tenant is privileged.
const PlatformSlug = "platform"

// ID is the tenant primary key (UUIDv7 string). Wrapper type prevents
// accidental swap with other domain ID types.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// nameMaxLen caps legal and display names to keep audit logs and UI predictable.
const nameMaxLen = 200

// Tenant is the aggregate root. Invariants enforced by [New] and state
// transition methods: ID and Slug non-zero; LegalName and DisplayName
// non-empty and ≤ nameMaxLen; Status follows the documented state machine.
//
// Admin email is NOT stored here (migration 20260507000008). It is a derived
// value — query the CompanyOwner-role membership. Storing it here would create
// a second source of truth that silently drifts after email-change flows.
type Tenant struct {
	id                  ID
	slug                slug.Slug
	legalName           string
	displayName         string
	status              Status
	statutory           Statutory          // optional Indian statutory IDs (GST/PAN/DrugLicence)
	adminContact        AdminContact       // optional admin phone + postal address
	settings            Settings           // password policy + future operational settings
	displayPreferences  DisplayPreferences // locale / time zone / date format / currency
	createdAt           time.Time
	activatedAt         time.Time // zero until first Activate
	suspendedAt         time.Time // zero until first Suspend; reset on subsequent Activate
	deletionScheduledAt time.Time // zero until MarkForDeletion; reset on RestoreFromDeletion
	deletionReason      string    // populated by MarkForDeletion; cleared on Restore
	hardDeletedAt       time.Time // zero until HardDelete; terminal
	events              []Event
}

// New constructs a brand-new Tenant in [StatusPending] and emits a
// [RegisteredEvent]. adminEmail is required for the event payload (welcome-
// email subscribers need it as a point-in-time fact) but is NOT stored on the
// aggregate (migration 20260507000008). Returns [ErrInvalid] on invariant
// violation.
func New(id ID, s slug.Slug, legalName, displayName string, adminEmail email.Address, now time.Time) (*Tenant, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if s.IsZero() {
		return nil, fmt.Errorf("%w: slug required", ErrInvalid)
	}
	if strings.TrimSpace(legalName) == "" {
		return nil, fmt.Errorf("%w: legal name required", ErrInvalid)
	}
	if strings.TrimSpace(displayName) == "" {
		return nil, fmt.Errorf("%w: display name required", ErrInvalid)
	}
	if len(legalName) > nameMaxLen {
		return nil, fmt.Errorf("%w: legal name too long (max %d, got %d)", ErrInvalid, nameMaxLen, len(legalName))
	}
	if len(displayName) > nameMaxLen {
		return nil, fmt.Errorf("%w: display name too long (max %d, got %d)", ErrInvalid, nameMaxLen, len(displayName))
	}
	if adminEmail.IsZero() {
		return nil, fmt.Errorf("%w: admin email required for RegisteredEvent payload", ErrInvalid)
	}

	now = now.UTC()
	t := &Tenant{
		id:          id,
		slug:        s,
		legalName:   legalName,
		displayName: displayName,
		status:      StatusPending,
		createdAt:   now,
	}
	t.recordEvent(RegisteredEvent{
		TenantID:    id,
		Slug:        s,
		LegalName:   legalName,
		DisplayName: displayName,
		AdminEmail:  adminEmail,
		At:          now,
	})
	return t, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// The adapter scans DB rows into this struct, keeping it free of internal
// Tenant field knowledge.
type Snapshot struct {
	ID                  ID
	Slug                slug.Slug
	LegalName           string
	DisplayName         string
	Status              Status
	Statutory           Statutory
	AdminContact        AdminContact
	Settings            Settings
	DisplayPreferences  DisplayPreferences
	CreatedAt           time.Time
	ActivatedAt         time.Time
	SuspendedAt         time.Time
	DeletionScheduledAt time.Time
	DeletionReason      string
	HardDeletedAt       time.Time
}

// UnmarshalFromDB re-hydrates a Tenant from persistence. Used only by the
// repository; does not re-validate invariants. Per TDL canon: data valid when
// stored must survive even if invariants later tighten.
func UnmarshalFromDB(s Snapshot) *Tenant {
	return &Tenant{
		id:                  s.ID,
		slug:                s.Slug,
		legalName:           s.LegalName,
		displayName:         s.DisplayName,
		status:              s.Status,
		statutory:           s.Statutory,
		adminContact:        s.AdminContact,
		settings:            s.Settings,
		displayPreferences:  s.DisplayPreferences,
		createdAt:           s.CreatedAt,
		activatedAt:         s.ActivatedAt,
		suspendedAt:         s.SuspendedAt,
		deletionScheduledAt: s.DeletionScheduledAt,
		deletionReason:      s.DeletionReason,
		hardDeletedAt:       s.HardDeletedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the tenant primary key.
func (t *Tenant) ID() ID { return t.id }

// Slug returns the URL-safe public handle.
func (t *Tenant) Slug() slug.Slug { return t.slug }

// LegalName returns the registered legal name.
func (t *Tenant) LegalName() string { return t.legalName }

// DisplayName returns the UI-facing friendly name.
func (t *Tenant) DisplayName() string { return t.displayName }

// Status returns the current lifecycle state.
func (t *Tenant) Status() Status { return t.status }

// CreatedAt returns the registration timestamp.
func (t *Tenant) CreatedAt() time.Time { return t.createdAt }

// ActivatedAt returns the most recent activation timestamp; zero if never activated.
func (t *Tenant) ActivatedAt() time.Time { return t.activatedAt }

// SuspendedAt returns the most recent suspension timestamp; zero if never suspended.
func (t *Tenant) SuspendedAt() time.Time { return t.suspendedAt }

// DeletionScheduledAt returns the start of the 30-day grace window; zero if
// not pending deletion or if Restore cleared the schedule.
func (t *Tenant) DeletionScheduledAt() time.Time { return t.deletionScheduledAt }

// DeletionReason returns the audit reason from MarkForDeletion; empty outside
// PendingDeletion/Deleted states.
func (t *Tenant) DeletionReason() string { return t.deletionReason }

// HardDeletedAt returns the terminal hard-delete timestamp; zero until Deleted.
func (t *Tenant) HardDeletedAt() time.Time { return t.hardDeletedAt }

// Statutory returns declared statutory IDs; zero means none declared yet.
func (t *Tenant) Statutory() Statutory { return t.statutory }

// AdminContact returns the admin phone + postal address; zero means not declared.
func (t *Tenant) AdminContact() AdminContact { return t.adminContact }

// Settings returns operational settings; zero means uninitialised — treat as
// DefaultPasswordPolicy at runtime.
func (t *Tenant) Settings() Settings { return t.settings }

// DisplayPreferences returns UI rendering preferences; zero means uninitialised —
// treat as DefaultDisplayPreferences at runtime.
func (t *Tenant) DisplayPreferences() DisplayPreferences { return t.displayPreferences }

// ----- State transitions ----------------------------------------------------

// UpdateProfile changes LegalName and DisplayName. Each attribute type has its
// own mutator; narrow events let subscribers react to exactly what changed.
// Idempotent: no event when both values are unchanged. Allowed in any status —
// suspended tenants may rename for legal reasons without re-activating.
func (t *Tenant) UpdateProfile(legalName, displayName string, now time.Time) error {
	if strings.TrimSpace(legalName) == "" {
		return fmt.Errorf("%w: legal name required", ErrInvalid)
	}
	if strings.TrimSpace(displayName) == "" {
		return fmt.Errorf("%w: display name required", ErrInvalid)
	}
	if len(legalName) > nameMaxLen {
		return fmt.Errorf("%w: legal name too long (max %d, got %d)", ErrInvalid, nameMaxLen, len(legalName))
	}
	if len(displayName) > nameMaxLen {
		return fmt.Errorf("%w: display name too long (max %d, got %d)", ErrInvalid, nameMaxLen, len(displayName))
	}
	if legalName == t.legalName && displayName == t.displayName {
		return nil
	}
	old := struct{ legal, display string }{t.legalName, t.displayName}
	t.legalName = legalName
	t.displayName = displayName
	t.recordEvent(ProfileUpdatedEvent{
		TenantID:       t.id,
		OldLegalName:   old.legal,
		OldDisplayName: old.display,
		NewLegalName:   legalName,
		NewDisplayName: displayName,
		At:             now.UTC(),
	})
	return nil
}

// Activate transitions the tenant to [StatusActive]. Idempotent if already
// active. Rejected from PendingDeletion (use RestoreFromDeletion) and Deleted
// (terminal). Emits [ActivatedEvent] and records the activation timestamp.
func (t *Tenant) Activate(now time.Time) error {
	if t.status == StatusActive {
		return nil // idempotent — TDL canon for already-correct state
	}
	if t.status == StatusPendingDeletion {
		return fmt.Errorf("%w: tenant pending deletion; use RestoreFromDeletion", ErrInvalid)
	}
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot activate", ErrInvalid)
	}
	now = now.UTC()
	t.status = StatusActive
	t.activatedAt = now
	t.recordEvent(ActivatedEvent{
		TenantID: t.id,
		At:       now,
	})
	return nil
}

// Suspend transitions the tenant to [StatusSuspended]. reason must be
// non-empty (audit requirement, data-retention.md). Idempotent if already
// suspended. Rejected from PendingDeletion and Deleted.
func (t *Tenant) Suspend(reason string, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: suspension reason required for audit", ErrInvalid)
	}
	if t.status == StatusSuspended {
		return nil
	}
	if t.status == StatusPendingDeletion {
		return fmt.Errorf("%w: tenant pending deletion; suspend not allowed", ErrInvalid)
	}
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot suspend", ErrInvalid)
	}
	now = now.UTC()
	t.status = StatusSuspended
	t.suspendedAt = now
	t.recordEvent(SuspendedEvent{
		TenantID: t.id,
		Reason:   reason,
		At:       now,
	})
	return nil
}

// UpdateStatutory replaces declared Indian statutory IDs with the supplied
// [Statutory] composite. Pass zero to clear all declarations. Validation
// (GST+PAN cross-check) already ran in [NewStatutory]; this method only
// enforces lifecycle rules. Idempotent when value is unchanged. Rejected
// from StatusDeleted.
func (t *Tenant) UpdateStatutory(s Statutory, now time.Time) error {
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot update statutory", ErrInvalid)
	}
	if t.statutory.Equal(s) {
		return nil
	}
	old := t.statutory
	t.statutory = s
	t.recordEvent(StatutoryUpdatedEvent{
		TenantID:     t.id,
		OldStatutory: old,
		NewStatutory: s,
		At:           now.UTC(),
	})
	return nil
}

// UpdateAdminContact replaces the admin phone + postal address. Pass zero to
// clear. Validation ran in the VO constructors; this method only enforces
// lifecycle rules. Idempotent when value is unchanged. Rejected from
// StatusDeleted.
func (t *Tenant) UpdateAdminContact(c AdminContact, now time.Time) error {
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot update admin contact", ErrInvalid)
	}
	if t.adminContact.Equal(c) {
		return nil
	}
	old := t.adminContact
	t.adminContact = c
	t.recordEvent(AdminContactUpdatedEvent{
		TenantID:        t.id,
		OldAdminContact: old,
		NewAdminContact: c,
		At:              now.UTC(),
	})
	return nil
}

// UpdateSettings replaces operational [Settings]. Validation ran in
// [NewPasswordPolicy]. Idempotent when value is unchanged. Rejected from
// StatusDeleted. Auth caches must react to [SettingsUpdatedEvent] to
// invalidate cached password policy.
func (t *Tenant) UpdateSettings(s Settings, now time.Time) error {
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot update settings", ErrInvalid)
	}
	if t.settings.Equal(s) {
		return nil
	}
	old := t.settings
	t.settings = s
	t.recordEvent(SettingsUpdatedEvent{
		TenantID:    t.id,
		OldSettings: old,
		NewSettings: s,
		At:          now.UTC(),
	})
	return nil
}

// UpdateDisplayPreferences replaces UI rendering preferences. Validation ran in
// [NewDisplayPreferences]. Idempotent when value is unchanged. Rejected from
// StatusDeleted. Subscribers (BFF, notification renderers) must invalidate
// cached preferences on receipt.
func (t *Tenant) UpdateDisplayPreferences(d DisplayPreferences, now time.Time) error {
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot update display preferences", ErrInvalid)
	}
	if t.displayPreferences.Equal(d) {
		return nil
	}
	old := t.displayPreferences
	t.displayPreferences = d
	t.recordEvent(DisplayPreferencesUpdatedEvent{
		TenantID:              t.id,
		OldDisplayPreferences: old,
		NewDisplayPreferences: d,
		At:                    now.UTC(),
	})
	return nil
}

// MarkForDeletion enters the 30-day grace window (data-retention.md, DPDP §12,
// SOC2 CC4.1). reason must be non-empty. Allowed from Active or Suspended.
// Idempotent when already PendingDeletion with the same reason; rejected on
// reason mismatch (audit-trail integrity). Rejected from Deleted (terminal)
// and Pending (never activated — use HardDelete directly).
func (t *Tenant) MarkForDeletion(reason string, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: deletion reason required for audit", ErrInvalid)
	}
	switch t.status {
	case StatusActive, StatusSuspended:
		// proceed
	case StatusPendingDeletion:
		// idempotent only when reason matches
		if t.deletionReason == reason {
			return nil
		}
		return fmt.Errorf("%w: tenant already pending deletion (reason: %q)", ErrInvalid, t.deletionReason)
	case StatusDeleted:
		return fmt.Errorf("%w: tenant already deleted", ErrInvalid)
	case StatusPending:
		return fmt.Errorf("%w: tenant never activated; cannot mark for deletion (use hard delete)", ErrInvalid)
	default:
		return fmt.Errorf("%w: invalid status %v", ErrInvalid, t.status)
	}
	now = now.UTC()
	t.status = StatusPendingDeletion
	t.deletionScheduledAt = now
	t.deletionReason = reason
	t.recordEvent(MarkedForDeletionEvent{
		TenantID:    t.id,
		Reason:      reason,
		ScheduledAt: now,
		At:          now,
	})
	return nil
}

// RestoreFromDeletion cancels a pending deletion and transitions back to Active.
// Idempotent from Active. Rejected from Pending, Suspended (not in the
// deletion pipeline), and Deleted (terminal).
func (t *Tenant) RestoreFromDeletion(now time.Time) error {
	switch t.status {
	case StatusActive:
		return nil // already restored — idempotent
	case StatusPendingDeletion:
		// proceed
	case StatusDeleted:
		return fmt.Errorf("%w: tenant already hard-deleted; cannot restore", ErrInvalid)
	default:
		return fmt.Errorf("%w: cannot restore from status %v", ErrInvalid, t.status)
	}
	now = now.UTC()
	t.status = StatusActive
	t.deletionScheduledAt = time.Time{}
	t.deletionReason = ""
	t.activatedAt = now
	t.recordEvent(RestoredEvent{
		TenantID: t.id,
		At:       now,
	})
	return nil
}

// HardDelete is the terminal transition fired by the data-retention saga.
// Allowed from PendingDeletion (saga path) and Pending (admin abandonment —
// no grace window needed since tenant never operated). Rejected from Active
// and Suspended (must MarkForDeletion first). Idempotent from Deleted.
// Emits [DeletedEvent]; subscribers anonymise PII per data-retention.md.
func (t *Tenant) HardDelete(now time.Time) error {
	switch t.status {
	case StatusDeleted:
		return nil // idempotent terminal
	case StatusPendingDeletion, StatusPending:
		// proceed
	default:
		return fmt.Errorf("%w: hard delete requires PendingDeletion or Pending status, got %v", ErrInvalid, t.status)
	}
	now = now.UTC()
	t.status = StatusDeleted
	t.hardDeletedAt = now
	t.recordEvent(DeletedEvent{
		TenantID: t.id,
		Reason:   t.deletionReason,
		At:       now,
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains and returns recorded domain events. The repository calls
// this inside the same transaction that persists state, then writes events to
// the outbox. Clears the internal slice; subsequent calls return nil until
// new mutations record events.
func (t *Tenant) PullEvents() []Event {
	if len(t.events) == 0 {
		return nil
	}
	out := t.events
	t.events = nil
	return out
}

func (t *Tenant) recordEvent(e Event) {
	t.events = append(t.events, e)
}
