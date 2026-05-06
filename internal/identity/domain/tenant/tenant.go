// Package tenant defines the Tenant aggregate — the global tenant entity.
//
// Per the Identity model in `multi-tenancy.md` doctrine + ADR 0002:
//   - Tenant is NOT itself tenant-scoped (each row IS a tenant).
//   - Identified by [ID] (UUIDv7) and addressed by [Slug] (URL-safe public handle).
//   - Lifecycle states: Pending → Active → Suspended ↔ Active.
//   - State transitions emit domain events (drained by repository on Save).
//
// All fields are private (sealed-type pattern). Construction is via
// [New] (factory enforcing invariants) or [UnmarshalFromDB] (repository-only
// re-hydration that does NOT re-validate, per TDL canon).
package tenant

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

// ErrInvalid is the sentinel returned (wrapped via %w) by [New] on invariant
// violation. Callers branch via [errors.Is] in error-mapping middleware.
var ErrInvalid = errs.New(errs.KindInvalidInput, "tenant", "invalid tenant")

// ID is the tenant primary key — UUIDv7 string for B-tree locality.
// Wrapper type prevents accidental swap with other domain IDs.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// nameMaxLen caps both legal + display names. RDBMS column is text but we
// cap at 200 chars to keep audit logs + UI predictable.
const nameMaxLen = 200

// Tenant is the aggregate root.
//
// Invariants (enforced by [New] + state transition methods):
//   - ID and Slug are non-zero.
//   - LegalName and DisplayName are non-empty + ≤ nameMaxLen.
//   - AdminEmail is a validated [email.Address].
//   - Status follows the documented state machine; transitions emit events.
type Tenant struct {
	id                  ID
	slug                slug.Slug
	legalName           string
	displayName         string
	adminEmail          email.Address
	status              Status
	statutory           Statutory // optional Indian statutory IDs (GST/PAN/DrugLicence)
	createdAt           time.Time
	activatedAt         time.Time // zero until first Activate
	suspendedAt         time.Time // zero until first Suspend; reset on subsequent Activate
	deletionScheduledAt time.Time // zero until MarkForDeletion; reset on RestoreFromDeletion
	deletionReason      string    // populated by MarkForDeletion; cleared on Restore
	hardDeletedAt       time.Time // zero until HardDelete; terminal
	events              []Event
}

// New constructs a brand-new tenant in [StatusPending].
//
// Returns [ErrInvalid] (wrapped with the specific failure) on invariant
// violation. On success, the tenant has emitted a [RegisteredEvent] which
// the repository drains via [PullEvents] when persisting.
func New(id ID, s slug.Slug, legalName, displayName string, adminEmail email.Address) (*Tenant, error) {
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
		return nil, fmt.Errorf("%w: admin email required", ErrInvalid)
	}

	now := clock.Now()
	t := &Tenant{
		id:          id,
		slug:        s,
		legalName:   legalName,
		displayName: displayName,
		adminEmail:  adminEmail,
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
//
// Adapter code (e.g. internal/identity/adapters/tenant_repository_pg.go)
// scans DB rows into this struct, then calls UnmarshalFromDB. Keeps the
// adapter free of internal Tenant field knowledge.
type Snapshot struct {
	ID                  ID
	Slug                slug.Slug
	LegalName           string
	DisplayName         string
	AdminEmail          email.Address
	Status              Status
	Statutory           Statutory
	CreatedAt           time.Time
	ActivatedAt         time.Time
	SuspendedAt         time.Time
	DeletionScheduledAt time.Time
	DeletionReason      string
	HardDeletedAt       time.Time
}

// UnmarshalFromDB re-hydrates a Tenant from persistence. Used ONLY by the
// repository on read paths — does NOT re-validate invariants.
//
// Per TDL canon (verified Wild Workouts Nov 2025): if data was valid when
// stored, re-validating on read could corrupt history when invariants
// tighten in code. Treat re-hydration as trusted I/O.
func UnmarshalFromDB(s Snapshot) *Tenant {
	return &Tenant{
		id:                  s.ID,
		slug:                s.Slug,
		legalName:           s.LegalName,
		displayName:         s.DisplayName,
		adminEmail:          s.AdminEmail,
		status:              s.Status,
		statutory:           s.Statutory,
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

// LegalName returns the registered legal name (e.g. "Acme Pharma Pvt Ltd").
func (t *Tenant) LegalName() string { return t.legalName }

// DisplayName returns the friendly name shown in UI.
func (t *Tenant) DisplayName() string { return t.displayName }

// AdminEmail returns the primary admin contact email.
func (t *Tenant) AdminEmail() email.Address { return t.adminEmail }

// Status returns the current lifecycle state.
func (t *Tenant) Status() Status { return t.status }

// CreatedAt returns the registration timestamp.
func (t *Tenant) CreatedAt() time.Time { return t.createdAt }

// ActivatedAt returns the most recent activation timestamp; zero if never activated.
func (t *Tenant) ActivatedAt() time.Time { return t.activatedAt }

// SuspendedAt returns the most recent suspension timestamp; zero if never suspended.
func (t *Tenant) SuspendedAt() time.Time { return t.suspendedAt }

// DeletionScheduledAt returns the timestamp at which deletion was
// scheduled (start of the 30-day grace window). Zero if not pending
// deletion or if Restore cleared the schedule.
func (t *Tenant) DeletionScheduledAt() time.Time { return t.deletionScheduledAt }

// DeletionReason returns the audit reason supplied when MarkForDeletion
// was called. Empty if not in PendingDeletion / Deleted state.
func (t *Tenant) DeletionReason() string { return t.deletionReason }

// HardDeletedAt returns the terminal hard-delete timestamp. Zero unless
// status == Deleted.
func (t *Tenant) HardDeletedAt() time.Time { return t.hardDeletedAt }

// Statutory returns the tenant's declared statutory IDs (GST/PAN/
// DrugLicence). Zero value means none declared yet.
func (t *Tenant) Statutory() Statutory { return t.statutory }

// ----- State transitions ----------------------------------------------------

// UpdateProfile changes the tenant's display fields (LegalName +
// DisplayName). Other tenant attributes (admin email, statutory IDs,
// settings) ride dedicated mutators per the .NET parent's vocabulary
// split — narrow events let subscribers react to the specific concern
// (audit, integration-event consumers, search-index reindexing) without
// guessing which fields actually changed.
//
// Validation mirrors [New]: both fields required, trimmed-non-empty,
// capped at nameMaxLen.
//
// Idempotent — calling with both fields equal to current values emits
// no event and returns nil. (Per coding-standards.md "no-op idempotency"
// + AssignManager / Activate precedent.)
//
// Allowed in any status — Suspended tenants can still rename for legal
// reasons (renamed entity, transferred ownership) without an Activate
// round-trip.
func (t *Tenant) UpdateProfile(legalName, displayName string) error {
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
		At:             clock.Now(),
	})
	return nil
}

// Activate transitions the tenant to [StatusActive] from any non-active state.
//
// Idempotent — if already active, returns nil and emits no event. Records
// the activation timestamp and emits [ActivatedEvent].
//
// Rejected from terminal/grace states: PendingDeletion (use
// RestoreFromDeletion instead) and Deleted (terminal).
func (t *Tenant) Activate() error {
	if t.status == StatusActive {
		return nil // idempotent — TDL canon for already-correct state
	}
	if t.status == StatusPendingDeletion {
		return fmt.Errorf("%w: tenant pending deletion; use RestoreFromDeletion", ErrInvalid)
	}
	if t.status == StatusDeleted {
		return fmt.Errorf("%w: tenant deleted; cannot activate", ErrInvalid)
	}
	now := clock.Now()
	t.status = StatusActive
	t.activatedAt = now
	t.recordEvent(ActivatedEvent{
		TenantID: t.id,
		At:       now,
	})
	return nil
}

// Suspend transitions the tenant to [StatusSuspended].
//
// reason MUST be non-empty (audit requirement per `data-retention.md`).
// Idempotent — if already suspended, returns nil with no event.
//
// Rejected from terminal/grace states: PendingDeletion (already in
// the deletion pipeline) and Deleted (terminal).
func (t *Tenant) Suspend(reason string) error {
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
	now := clock.Now()
	t.status = StatusSuspended
	t.suspendedAt = now
	t.recordEvent(SuspendedEvent{
		TenantID: t.id,
		Reason:   reason,
		At:       now,
	})
	return nil
}

// UpdateStatutory replaces the tenant's declared Indian statutory IDs
// (GST/PAN/DrugLicence) with a new [Statutory] composite. Pass a zero
// [Statutory] to clear all declarations (rare — typically used during
// onboarding-correction or post-suspension audit).
//
// Validation already happened inside [NewStatutory] (cross-checks PAN
// embedded in GST against supplied PAN). This method is the
// state-mutation entry point — it only enforces tenant-lifecycle rules.
//
// Idempotent: no-op + no event when the new value equals the current.
//
// Allowed in any non-terminal status (Pending, Active, Suspended,
// PendingDeletion). Rejected from StatusDeleted (terminal).
func (t *Tenant) UpdateStatutory(s Statutory) error {
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
		At:           clock.Now(),
	})
	return nil
}

// MarkForDeletion enters the 30-day grace window per `data-retention.md`
// "Tenant deletion saga". Operator action; reason MUST be non-empty
// for audit (DPDP §12, SOC2 CC4.1).
//
// Allowed transitions:
//   - StatusActive → StatusPendingDeletion
//   - StatusSuspended → StatusPendingDeletion
//
// Rejected:
//   - StatusPendingDeletion → MarkForDeletion is idempotent only when
//     reason matches the existing schedule's reason — otherwise rejected
//     (audit-trail integrity).
//   - StatusDeleted (terminal) → cannot re-mark.
//   - StatusPending → tenant never activated; just hard-delete instead.
//
// Records the schedule timestamp + reason; emits MarkedForDeletionEvent.
func (t *Tenant) MarkForDeletion(reason string) error {
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
	now := clock.Now()
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

// RestoreFromDeletion cancels a pending deletion within the grace
// window. Transitions PendingDeletion → Active and clears the
// scheduled-deletion fields.
//
// Idempotent only when status is already Active (no-op). Rejected
// from any other status — caller can't restore from Pending /
// Suspended (those aren't "deletion" states) or Deleted (terminal).
func (t *Tenant) RestoreFromDeletion() error {
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
	now := clock.Now()
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

// HardDelete is the terminal transition fired by the data-retention
// saga after the 30-day grace window expires.
//
// Allowed transitions:
//   - StatusPendingDeletion → StatusDeleted (saga path)
//   - StatusPending → StatusDeleted (admin abandonment of un-activated
//     tenant; no grace window needed since tenant never operated)
//
// Rejected from Active / Suspended (must MarkForDeletion first) and
// Deleted (idempotent on terminal state via early return).
//
// Emits DeletedEvent; subscribers anonymise PII per `data-retention.md`.
func (t *Tenant) HardDelete() error {
	switch t.status {
	case StatusDeleted:
		return nil // idempotent terminal
	case StatusPendingDeletion, StatusPending:
		// proceed
	default:
		return fmt.Errorf("%w: hard delete requires PendingDeletion or Pending status, got %v", ErrInvalid, t.status)
	}
	now := clock.Now()
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

// PullEvents drains the recorded domain events and returns them.
// The repository calls this once per Save inside the same transaction
// that persists state, then forwards events to the outbox.
//
// After PullEvents, the internal slice is cleared — subsequent calls
// return nil until new mutations record events.
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
