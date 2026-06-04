// Package reminder defines the Reminder aggregate — the CRM module's
// notification surface per BRD §4.6.
//
// Reminder sources (BRD §4.5, §4.6, §4.7):
//
//   - [TypeCallback]   — auto-created by the CallLogged subscriber when
//     a caller records callback_window_start_at on the call log
//     ("contact was busy, call again at X").
//   - [TypeMatureLead] — auto-created daily by [MatureLeadScan] (BRD §4.7)
//     for converted leads with no reorder activity in the last 3 months.
//   - [TypeManual]     — created via the HTTP surface by the assigned
//     sales executive or their manager.
//
// State machine (strict):
//
//	pending → sent      (terminal — user marked it fired)
//	pending → cancelled (terminal — user cancelled with a required reason)
//
// Tenant scoping (ADR 0062 — TDL canon): every state-mutating method
// rejects the aggregate when its tenant_id does not match the caller's
// scope at the SQL layer. Domain code carries TenantID as an explicit
// VO; ctx is never read.
package reminder

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// validateUUIDString returns ErrInvalid wrapping a clear message when
// `val` is not a RFC 9562 UUID. Empty input is REJECTED — callers with
// optional UUID fields should pass through [validateOptionalUUID].
func validateUUIDString(name, val string) error {
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// validateOptionalUUID is the empty-allowed variant. Returns nil if
// val is empty; otherwise validates per [validateUUIDString].
func validateOptionalUUID(name, val string) error {
	if val == "" {
		return nil
	}
	return validateUUIDString(name, val)
}

// ErrInvalid is the sentinel returned (wrapped via %w) by every factory
// + mutator on invariant violation. Callers branch via [errors.Is] in
// error-mapping middleware.
var ErrInvalid = errs.New(errs.KindInvalidInput, "reminder", "invalid reminder")

// ErrConflict is returned by [Reminder.MarkSent] / [Reminder.Cancel]
// when the reminder is already in a terminal state. Callers map to 409
// Conflict.
var ErrConflict = errs.New(errs.KindConflict, "reminder", "reminder is in a terminal state")

// ID is the reminder primary key — UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

const (
	// notesMaxLen mirrors the CHECK constraint on crm.reminders.notes
	// in the slice-1 migration.
	notesMaxLen = 2000

	// cancelReasonMaxLen mirrors the CHECK constraint on
	// crm.reminders.cancel_reason.
	cancelReasonMaxLen = 1000
)

// Reminder is the aggregate root.
//
// Invariants (enforced by factories + state-transition methods):
//   - ID + TenantID + LeadID + AssignedToMembershipID non-zero.
//   - Type is a valid catalogue entry.
//   - State transitions follow the strict state machine: only Pending
//     reminders may be marked sent or cancelled.
//   - DueAt non-zero.
//   - For [TypeCallback], SourceCallLogID is REQUIRED + must be a UUID
//     (drives the at-most-one-pending-per-call partial unique index).
//   - For [TypeMatureLead] + [TypeManual], SourceCallLogID is empty.
type Reminder struct {
	id                     ID
	tenantID               tenant.ID
	leadID                 crmlead.ID
	assignedToMembershipID string
	createdByMembershipID  string // empty for subscriber / cron-created reminders
	sourceCallLogID        string // populated only for TypeCallback

	rtype Type
	state State

	dueAt   time.Time
	notes   string

	// Terminal-state metadata.
	sentAt                  time.Time
	markedSentByMembershipID string
	cancelledAt             time.Time
	cancelledByMembershipID string
	cancelReason            string

	createdAt time.Time

	events []Event
}

// NewCallbackReminder is the subscriber-side factory used by the
// CallLogged subscriber when the call carries a non-zero
// callback_window_start_at (BRD §4.5).
//
// `sourceCallLogID` must be a valid UUID — it's the natural-key the
// at-most-one-pending-per-call partial unique index uses. `createdBy`
// may be empty for the subscriber path; the underlying call-log's
// LoggedBy is the audit anchor.
func NewCallbackReminder(
	id ID,
	tenantID tenant.ID,
	leadID crmlead.ID,
	assignedTo string,
	createdBy string,
	sourceCallLogID string,
	dueAt time.Time,
	notes string,
	now time.Time,
) (*Reminder, error) {
	if err := validateUUIDString("source call log id", strings.TrimSpace(sourceCallLogID)); err != nil {
		return nil, err
	}
	return newReminder(id, tenantID, leadID, assignedTo, createdBy, sourceCallLogID,
		TypeCallback, dueAt, notes, now)
}

// NewMatureLeadReminder is the daily-scan factory used by the
// MatureLeadScan river job (BRD §4.7) for converted leads with no
// reorder activity in the configured window.
//
// `createdBy` is empty — these are system-created reminders. The
// assigned-to membership is the lead's current assignee.
func NewMatureLeadReminder(
	id ID,
	tenantID tenant.ID,
	leadID crmlead.ID,
	assignedTo string,
	dueAt time.Time,
	notes string,
	now time.Time,
) (*Reminder, error) {
	return newReminder(id, tenantID, leadID, assignedTo, "", "",
		TypeMatureLead, dueAt, notes, now)
}

// NewManualReminder is the HTTP-path factory. The user supplies the
// assignee + due date + optional notes.
func NewManualReminder(
	id ID,
	tenantID tenant.ID,
	leadID crmlead.ID,
	assignedTo string,
	createdBy string,
	dueAt time.Time,
	notes string,
	now time.Time,
) (*Reminder, error) {
	if err := validateUUIDString("created by membership id", strings.TrimSpace(createdBy)); err != nil {
		return nil, err
	}
	return newReminder(id, tenantID, leadID, assignedTo, createdBy, "",
		TypeManual, dueAt, notes, now)
}

// newReminder is the shared invariant gate behind every public factory.
// Returns a [*Reminder] in [StatePending] that has emitted a single
// [CreatedEvent] (drained by the repository on Add).
func newReminder(
	id ID,
	tenantID tenant.ID,
	leadID crmlead.ID,
	assignedTo string,
	createdBy string,
	sourceCallLogID string,
	rtype Type,
	dueAt time.Time,
	notes string,
	now time.Time,
) (*Reminder, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if err := validateUUIDString("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUIDString("tenant id", strings.TrimSpace(tenantID.String())); err != nil {
		return nil, err
	}
	if leadID.IsZero() {
		return nil, fmt.Errorf("%w: lead id required", ErrInvalid)
	}
	if err := validateUUIDString("lead id", leadID.String()); err != nil {
		return nil, err
	}
	if err := validateUUIDString("assigned to membership id", strings.TrimSpace(assignedTo)); err != nil {
		return nil, err
	}
	if err := validateOptionalUUID("created by membership id", createdBy); err != nil {
		return nil, err
	}
	if !rtype.IsValid() {
		return nil, fmt.Errorf("%w: type %q invalid", ErrInvalid, rtype)
	}
	if dueAt.IsZero() {
		return nil, fmt.Errorf("%w: due_at required", ErrInvalid)
	}
	if len(notes) > notesMaxLen {
		return nil, fmt.Errorf("%w: notes too long (max %d, got %d)", ErrInvalid, notesMaxLen, len(notes))
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	r := &Reminder{
		id:                     id,
		tenantID:               tenantID,
		leadID:                 leadID,
		assignedToMembershipID: assignedTo,
		createdByMembershipID:  createdBy,
		sourceCallLogID:        sourceCallLogID,
		rtype:                  rtype,
		state:                  StatePending,
		dueAt:                  dueAt.UTC(),
		notes:                  notes,
		createdAt:              now.UTC(),
	}
	r.recordEvent(CreatedEvent{
		ReminderID:             id,
		TenantID:               tenantID,
		LeadID:                 leadID,
		AssignedToMembershipID: assignedTo,
		Type:                   rtype,
		DueAt:                  dueAt.UTC(),
		SourceCallLogID:        sourceCallLogID,
		CreatedByMembershipID:  createdBy,
		At:                     now.UTC(),
	})
	return r, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// Adapter code scans DB rows into this struct, then re-hydrates via
// [UnmarshalFromDB] — keeps the adapter free of internal field knowledge.
type Snapshot struct {
	ID                       ID
	TenantID                 tenant.ID
	LeadID                   crmlead.ID
	AssignedToMembershipID   string
	CreatedByMembershipID    string
	SourceCallLogID          string
	Type                     Type
	State                    State
	DueAt                    time.Time
	Notes                    string
	SentAt                   time.Time
	MarkedSentByMembershipID string
	CancelledAt              time.Time
	CancelledByMembershipID  string
	CancelReason             string
	CreatedAt                time.Time
}

// UnmarshalFromDB re-hydrates a Reminder from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate invariants per
// TDL canon (Wild Workouts Nov 2025).
func UnmarshalFromDB(s Snapshot) *Reminder {
	return &Reminder{
		id:                       s.ID,
		tenantID:                 s.TenantID,
		leadID:                   s.LeadID,
		assignedToMembershipID:   s.AssignedToMembershipID,
		createdByMembershipID:    s.CreatedByMembershipID,
		sourceCallLogID:          s.SourceCallLogID,
		rtype:                    s.Type,
		state:                    s.State,
		dueAt:                    s.DueAt,
		notes:                    s.Notes,
		sentAt:                   s.SentAt,
		markedSentByMembershipID: s.MarkedSentByMembershipID,
		cancelledAt:              s.CancelledAt,
		cancelledByMembershipID:  s.CancelledByMembershipID,
		cancelReason:             s.CancelReason,
		createdAt:                s.CreatedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the reminder primary key.
func (r *Reminder) ID() ID { return r.id }

// TenantID returns the owning tenant.
func (r *Reminder) TenantID() tenant.ID { return r.tenantID }

// LeadID returns the parent lead.
func (r *Reminder) LeadID() crmlead.ID { return r.leadID }

// AssignedToMembershipID returns the membership the reminder is for.
func (r *Reminder) AssignedToMembershipID() string { return r.assignedToMembershipID }

// CreatedByMembershipID returns the actor who created the reminder, or
// empty for subscriber / cron-created paths.
func (r *Reminder) CreatedByMembershipID() string { return r.createdByMembershipID }

// SourceCallLogID returns the parent call-log for [TypeCallback]
// reminders, or empty for other types.
func (r *Reminder) SourceCallLogID() string { return r.sourceCallLogID }

// Type returns the reminder type.
func (r *Reminder) Type() Type { return r.rtype }

// State returns the current lifecycle state.
func (r *Reminder) State() State { return r.state }

// DueAt returns the wall-clock the reminder is due at.
func (r *Reminder) DueAt() time.Time { return r.dueAt }

// Notes returns the optional free-text notes.
func (r *Reminder) Notes() string { return r.notes }

// SentAt returns the terminal-Sent timestamp; zero when not in
// [StateSent].
func (r *Reminder) SentAt() time.Time { return r.sentAt }

// MarkedSentByMembershipID returns the actor who marked the reminder
// sent; empty when not in [StateSent].
func (r *Reminder) MarkedSentByMembershipID() string { return r.markedSentByMembershipID }

// CancelledAt returns the terminal-Cancelled timestamp; zero when not
// in [StateCancelled].
func (r *Reminder) CancelledAt() time.Time { return r.cancelledAt }

// CancelledByMembershipID returns the actor who cancelled the reminder;
// empty when not in [StateCancelled].
func (r *Reminder) CancelledByMembershipID() string { return r.cancelledByMembershipID }

// CancelReason returns the audit reason supplied at cancellation.
func (r *Reminder) CancelReason() string { return r.cancelReason }

// CreatedAt returns the row-insert timestamp.
func (r *Reminder) CreatedAt() time.Time { return r.createdAt }

// ----- State transitions ----------------------------------------------------

// MarkSent transitions the reminder to [StateSent]. Idempotent on
// self-transition is REJECTED — once Sent, MarkSent again returns
// [ErrConflict] (the dashboard would otherwise re-fire). Refuses when
// the reminder is already in [StateCancelled].
//
// `markedBy` is the actor (the assignee or their manager); REQUIRED.
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (r *Reminder) MarkSent(markedBy string, now time.Time) error {
	if err := validateUUIDString("marked-by membership id", strings.TrimSpace(markedBy)); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if r.state.IsTerminal() {
		return fmt.Errorf("%w: state=%s; mark-sent not allowed", ErrConflict, r.state)
	}
	r.state = StateSent
	r.sentAt = now.UTC()
	r.markedSentByMembershipID = markedBy
	r.recordEvent(MarkedSentEvent{
		ReminderID:          r.id,
		TenantID:            r.tenantID,
		LeadID:              r.leadID,
		MarkedByMembershipID: markedBy,
		At:                  now.UTC(),
	})
	return nil
}

// Cancel transitions the reminder to [StateCancelled]. Refuses when the
// reminder is already in any terminal state.
//
// `reason` is REQUIRED (audit doctrine — data-retention.md); empty
// returns [ErrInvalid].
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (r *Reminder) Cancel(cancelledBy, reason string, now time.Time) error {
	if err := validateUUIDString("cancelled-by membership id", strings.TrimSpace(cancelledBy)); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: cancel reason required for audit", ErrInvalid)
	}
	if len(reason) > cancelReasonMaxLen {
		return fmt.Errorf("%w: cancel reason too long (max %d, got %d)", ErrInvalid, cancelReasonMaxLen, len(reason))
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if r.state.IsTerminal() {
		return fmt.Errorf("%w: state=%s; cancel not allowed", ErrConflict, r.state)
	}
	r.state = StateCancelled
	r.cancelledAt = now.UTC()
	r.cancelledByMembershipID = cancelledBy
	r.cancelReason = reason
	r.recordEvent(CancelledEvent{
		ReminderID:              r.id,
		TenantID:                r.tenantID,
		LeadID:                  r.leadID,
		CancelledByMembershipID: cancelledBy,
		Reason:                  reason,
		At:                      now.UTC(),
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded domain events and returns them. The
// repository calls this once per Save inside the same transaction that
// persists state, then forwards events to the outbox.
func (r *Reminder) PullEvents() []Event {
	if len(r.events) == 0 {
		return nil
	}
	out := r.events
	r.events = nil
	return out
}

func (r *Reminder) recordEvent(e Event) {
	r.events = append(r.events, e)
}
