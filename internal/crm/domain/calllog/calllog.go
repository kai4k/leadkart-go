// Package calllog defines the CallLog aggregate per ADR 0060.
//
// CallLog is the append-only call audit for a CrmLead. Each row
// captures one interaction (call attempt, outcome, optional notes).
//
// Aggregate boundary discipline (per ADR 0060): CallLog is INDEPENDENT
// of CrmLead — splitting them keeps row-lock pressure off the lead
// during high-call-volume sales activity. The CallLog references the
// lead by ID only; no FK navigation property.
package calllog

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// validateUUIDString returns ErrInvalid wrapping a clear message when
// `val` is not a RFC 9562 UUID. Per ADR 0060 + reviewer finding H6:
// validation lives at AGGREGATE-CONSTRUCTION time, not later at the
// outbox boundary.
func validateUUIDString(name, val string) error {
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// ErrInvalid is the sentinel returned (wrapped via %w) by [New] on
// invariant violation.
var ErrInvalid = errs.New(errs.KindInvalidInput, "call_log", "invalid call log")

// ID is the call-log primary key — UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Outcome enumerates the recognised call outcomes. Mirrors the CHECK
// constraint on crm.call_logs.outcome (migration 20260602000001).
type Outcome string

// Outcome catalogue values. Wire-stable strings — match the CHECK
// constraint on crm.call_logs.outcome in migration 20260602000001.
const (
	OutcomeConnected         Outcome = "connected"
	OutcomeNoAnswer          Outcome = "no_answer"
	OutcomeBusy              Outcome = "busy"
	OutcomeWrongNumber       Outcome = "wrong_number"
	OutcomeNotInterested     Outcome = "not_interested"
	OutcomeInterested        Outcome = "interested"
	OutcomeCallbackRequested Outcome = "callback_requested"
	OutcomeConverted         Outcome = "converted"
	OutcomeOther             Outcome = "other"
)

// String returns the wire form.
func (o Outcome) String() string { return string(o) }

// IsValid reports whether o is a known catalogue entry.
func (o Outcome) IsValid() bool {
	switch o {
	case OutcomeConnected, OutcomeNoAnswer, OutcomeBusy, OutcomeWrongNumber,
		OutcomeNotInterested, OutcomeInterested, OutcomeCallbackRequested,
		OutcomeConverted, OutcomeOther:
		return true
	}
	return false
}

// ParseOutcome turns an untrusted string into an [Outcome] or returns
// [ErrInvalid] wrapped with the bad value.
func ParseOutcome(raw string) (Outcome, error) {
	o := Outcome(raw)
	if !o.IsValid() {
		return "", fmt.Errorf("%w: outcome %q not in catalogue", ErrInvalid, raw)
	}
	return o, nil
}

const notesMaxLen = 4000

// CallLog is the aggregate root. Append-only — exposes [Add] via the
// repository but has NO state-mutation methods after [New].
//
// Invariants enforced by [New]:
//   - ID + TenantID + LeadID non-zero.
//   - Outcome is a valid catalogue entry.
//   - LoggedByMembershipID non-empty.
//   - Notes ≤ notesMaxLen.
//   - LoggedAt non-zero (caller supplies — typically time.Now()).
type CallLog struct {
	id                   ID
	tenantID             string
	leadID               crmlead.ID
	outcome              Outcome
	notes                string
	loggedByMembershipID string
	loggedAt             time.Time
	createdAt            time.Time

	events []Event
}

// New constructs a brand-new CallLog + emits [LoggedEvent]. The repository
// drains the event via [PullEvents] when persisting.
//
// loggedAt MUST be non-zero — caller passes the wall-clock time the
// call happened (typically time.Now()).
func New(id ID, tenantID string, leadID crmlead.ID, outcome Outcome, notes, loggedBy string, loggedAt time.Time) (*CallLog, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if err := validateUUIDString("id", string(id)); err != nil {
		return nil, err
	}
	if err := validateUUIDString("tenant id", strings.TrimSpace(tenantID)); err != nil {
		return nil, err
	}
	if leadID.IsZero() {
		return nil, fmt.Errorf("%w: lead id required", ErrInvalid)
	}
	if err := validateUUIDString("lead id", leadID.String()); err != nil {
		return nil, err
	}
	if !outcome.IsValid() {
		return nil, fmt.Errorf("%w: outcome %q invalid", ErrInvalid, outcome)
	}
	if err := validateUUIDString("logged-by membership id", strings.TrimSpace(loggedBy)); err != nil {
		return nil, err
	}
	if len(notes) > notesMaxLen {
		return nil, fmt.Errorf("%w: notes too long (max %d, got %d)", ErrInvalid, notesMaxLen, len(notes))
	}
	if loggedAt.IsZero() {
		return nil, fmt.Errorf("%w: logged_at required", ErrInvalid)
	}
	now := clock.Now()
	c := &CallLog{
		id:                   id,
		tenantID:             tenantID,
		leadID:               leadID,
		outcome:              outcome,
		notes:                notes,
		loggedByMembershipID: loggedBy,
		loggedAt:             loggedAt,
		createdAt:            now,
	}
	c.recordEvent(LoggedEvent{
		CallID:               id,
		LeadID:               leadID,
		TenantID:             tenantID,
		Outcome:              outcome,
		LoggedByMembershipID: loggedBy,
		At:                   loggedAt,
	})
	return c, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                   ID
	TenantID             string
	LeadID               crmlead.ID
	Outcome              Outcome
	Notes                string
	LoggedByMembershipID string
	LoggedAt             time.Time
	CreatedAt            time.Time
}

// UnmarshalFromDB re-hydrates a CallLog from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate invariants.
func UnmarshalFromDB(s Snapshot) *CallLog {
	return &CallLog{
		id:                   s.ID,
		tenantID:             s.TenantID,
		leadID:               s.LeadID,
		outcome:              s.Outcome,
		notes:                s.Notes,
		loggedByMembershipID: s.LoggedByMembershipID,
		loggedAt:             s.LoggedAt,
		createdAt:            s.CreatedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the call-log primary key.
func (c *CallLog) ID() ID { return c.id }

// TenantID returns the owning tenant.
func (c *CallLog) TenantID() string { return c.tenantID }

// LeadID returns the parent lead.
func (c *CallLog) LeadID() crmlead.ID { return c.leadID }

// Outcome returns the call outcome.
func (c *CallLog) Outcome() Outcome { return c.outcome }

// Notes returns the optional free-text notes.
func (c *CallLog) Notes() string { return c.notes }

// LoggedByMembershipID returns the actor who logged the call.
func (c *CallLog) LoggedByMembershipID() string { return c.loggedByMembershipID }

// LoggedAt returns the wall-clock time the call happened.
func (c *CallLog) LoggedAt() time.Time { return c.loggedAt }

// CreatedAt returns the row-insert timestamp.
func (c *CallLog) CreatedAt() time.Time { return c.createdAt }

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded domain events and returns them.
func (c *CallLog) PullEvents() []Event {
	if len(c.events) == 0 {
		return nil
	}
	out := c.events
	c.events = nil
	return out
}

func (c *CallLog) recordEvent(e Event) {
	c.events = append(c.events, e)
}
