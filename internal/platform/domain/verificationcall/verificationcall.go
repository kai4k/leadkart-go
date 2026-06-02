// Package verificationcall defines the VerificationCall aggregate: an
// append-only call-log entry (BRD §6.2, ADR 0059).
//
// Its own aggregate, not an entity inside UnverifiedContact: calls are
// created independently (many per contact across re-attempts) with no
// shared invariant, are append-only, and the contact's state transition
// is orchestrated by the handler in the same tx rather than forced into
// one boundary (Vernon IDDD ch.10).
//
// Platform-only (no tenant_id).
package verificationcall

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// ErrInvalid is the sentinel for invariant violations.
var ErrInvalid = errors.New("verificationcall: invalid")

// ID is the call-log row primary key (UUIDv7 string).
type ID string

// IsZero reports whether i is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// OutcomeCode is the closed-set call outcome. The contact handler maps it
// to a transition (MarkVerified/MarkRejected/MarkBusy); NoAnswer and
// WrongNumber carry no transition and leave the contact unchanged.
type OutcomeCode string

// Closed-set call [OutcomeCode] values per ADR 0059.
const (
	OutcomeVerified    OutcomeCode = "verified"
	OutcomeRejected    OutcomeCode = "rejected"
	OutcomeBusy        OutcomeCode = "busy"
	OutcomeNoAnswer    OutcomeCode = "no_answer"
	OutcomeWrongNumber OutcomeCode = "wrong_number"
)

// IsValid reports whether o is one of the closed-set outcomes.
func (o OutcomeCode) IsValid() bool {
	switch o {
	case OutcomeVerified, OutcomeRejected, OutcomeBusy, OutcomeNoAnswer, OutcomeWrongNumber:
		return true
	}
	return false
}

// String returns the wire form.
func (o OutcomeCode) String() string { return string(o) }

// VerificationCall is the aggregate root. Immutable once constructed.
type VerificationCall struct {
	id                    ID
	contactID             unverifiedcontact.ID
	outcome               OutcomeCode
	notes                 string
	callbackWindowStartAt time.Time
	callbackWindowEndAt   time.Time
	loggedAt              time.Time
	loggedByMembershipID  unverifiedcontact.MembershipID

	events []Event
}

// New constructs a VerificationCall, enforcing: id/contactID/loggedBy
// non-zero, outcome in the closed set, and for Busy a valid callback
// window (both timestamps set, end after start; mirrors
// UnverifiedContact.MarkBusy).
func New(
	id ID,
	contactID unverifiedcontact.ID,
	outcome OutcomeCode,
	notes string,
	callbackWindowStartAt, callbackWindowEndAt time.Time,
	loggedBy unverifiedcontact.MembershipID,
	now time.Time,
) (*VerificationCall, error) {
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("contactID", contactID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("loggedBy", loggedBy.String()); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	if !outcome.IsValid() {
		return nil, fmt.Errorf("%w: outcome %q invalid", ErrInvalid, outcome)
	}

	if outcome == OutcomeBusy {
		if callbackWindowStartAt.IsZero() || callbackWindowEndAt.IsZero() {
			return nil, fmt.Errorf("%w: busy outcome requires callback window", ErrInvalid)
		}
		if !callbackWindowEndAt.After(callbackWindowStartAt) {
			return nil, fmt.Errorf("%w: callback end must be after start", ErrInvalid)
		}
	}
	// Non-busy outcomes must not carry a callback window: rejects
	// frontend that submits both regardless of outcome.
	if outcome != OutcomeBusy && (!callbackWindowStartAt.IsZero() || !callbackWindowEndAt.IsZero()) {
		return nil, fmt.Errorf("%w: non-busy outcome must not carry callback window", ErrInvalid)
	}

	c := &VerificationCall{
		id:                    id,
		contactID:             contactID,
		outcome:               outcome,
		notes:                 strings.TrimSpace(notes),
		callbackWindowStartAt: callbackWindowStartAt,
		callbackWindowEndAt:   callbackWindowEndAt,
		loggedAt:              now,
		loggedByMembershipID:  loggedBy,
	}
	c.recordEvent(LoggedEvent{
		CallID:               id,
		ContactID:            contactID,
		OutcomeCode:          outcome,
		LoggedAt:             now,
		LoggedByMembershipID: loggedBy,
	})
	return c, nil
}

// validateUUID enforces the H6 reviewer rule: every domain ID must parse
// as a UUID at AGGREGATE-CONSTRUCTION time, not later at the adapter
// boundary. Trims surrounding whitespace before parsing.
func validateUUID(name, val string) error {
	val = strings.TrimSpace(val)
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                    ID
	ContactID             unverifiedcontact.ID
	Outcome               OutcomeCode
	Notes                 string
	CallbackWindowStartAt time.Time
	CallbackWindowEndAt   time.Time
	LoggedAt              time.Time
	LoggedByMembershipID  unverifiedcontact.MembershipID
}

// UnmarshalFromDB rehydrates the aggregate without re-validating.
func UnmarshalFromDB(s Snapshot) *VerificationCall {
	return &VerificationCall{
		id:                    s.ID,
		contactID:             s.ContactID,
		outcome:               s.Outcome,
		notes:                 s.Notes,
		callbackWindowStartAt: s.CallbackWindowStartAt,
		callbackWindowEndAt:   s.CallbackWindowEndAt,
		loggedAt:              s.LoggedAt,
		loggedByMembershipID:  s.LoggedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the row primary key.
func (c *VerificationCall) ID() ID { return c.id }

// ContactID returns the parent UnverifiedContact FK.
func (c *VerificationCall) ContactID() unverifiedcontact.ID { return c.contactID }

// Outcome returns the closed-set call outcome.
func (c *VerificationCall) Outcome() OutcomeCode { return c.outcome }

// Notes returns the free-text Lead Agent notes.
func (c *VerificationCall) Notes() string { return c.notes }

// CallbackWindowStartAt returns the callback window start; zero unless Busy.
func (c *VerificationCall) CallbackWindowStartAt() time.Time { return c.callbackWindowStartAt }

// CallbackWindowEndAt returns the callback window end; zero unless Busy.
func (c *VerificationCall) CallbackWindowEndAt() time.Time { return c.callbackWindowEndAt }

// LoggedAt returns the call-log timestamp.
func (c *VerificationCall) LoggedAt() time.Time { return c.loggedAt }

// LoggedByMembershipID returns the Lead Agent's membership.
func (c *VerificationCall) LoggedByMembershipID() unverifiedcontact.MembershipID {
	return c.loggedByMembershipID
}

// ----- Events --------------------------------------------------------------

// PullEvents drains and returns the recorded domain events.
func (c *VerificationCall) PullEvents() []Event {
	if len(c.events) == 0 {
		return nil
	}
	out := c.events
	c.events = nil
	return out
}

func (c *VerificationCall) recordEvent(e Event) {
	c.events = append(c.events, e)
}

// Event is the sealed marker interface.
type Event interface{ isVerificationCallEvent() }

// LoggedEvent fires on [New]; one per call insert. Mirrors the .NET
// VerificationCallLogged integration event.
type LoggedEvent struct {
	CallID               ID
	ContactID            unverifiedcontact.ID
	OutcomeCode          OutcomeCode
	LoggedAt             time.Time
	LoggedByMembershipID unverifiedcontact.MembershipID
}

func (LoggedEvent) isVerificationCallEvent() {}
