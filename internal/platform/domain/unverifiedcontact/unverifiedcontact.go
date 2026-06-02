// Package unverifiedcontact defines the UnverifiedContact aggregate —
// the Lead Agent's work queue per BRD §6.2 + ADR 0059.
//
// State machine:
//
//	New ──StartCall──▶ InCall ──MarkVerified──▶ Verified (terminal)
//	                    │
//	                    ├──MarkRejected──▶ Rejected (terminal)
//	                    │
//	                    └──MarkBusy(callback)──▶ Busy
//	                                                │
//	                                                └──StartCall──▶ InCall (re-attempt)
//
// Transitions emit domain events drained via PullEvents on persist.
//
// Per ADR 0059 the contact is Platform-only (no tenant_id); BRD §5
// lead-form fields live on the contained [leadform.Form] VO.
package unverifiedcontact

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
)

// ErrInvalid is wrapped and returned on invariant violation.
var ErrInvalid = errors.New("unverifiedcontact: invalid")

// ID is the UnverifiedContact primary key (UUIDv7 string).
type ID string

// IsZero reports whether i is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// State is the lifecycle marker.
type State string

// Closed-set [State] values per ADR 0059.
const (
	StateNew      State = "new"
	StateInCall   State = "in_call"
	StateVerified State = "verified"
	StateRejected State = "rejected"
	StateBusy     State = "busy"
)

// IsValid reports whether s is a known state.
func (s State) IsValid() bool {
	switch s {
	case StateNew, StateInCall, StateVerified, StateRejected, StateBusy:
		return true
	}
	return false
}

// String returns the wire form.
func (s State) String() string { return string(s) }

// MembershipID is the FK to identity.tenant_memberships.id. A string,
// not the foreign type, to avoid a cross-module domain import.
type MembershipID string

// IsZero reports whether m is unset.
func (m MembershipID) IsZero() bool { return m == "" }

// String returns the underlying UUID string.
func (m MembershipID) String() string { return string(m) }

// UnverifiedContact is the aggregate root.
type UnverifiedContact struct {
	id                ID
	form              leadform.Form
	state             State
	rejectionReason   string
	busyCallbackAt    time.Time // window start
	busyCallbackEndAt time.Time // window end
	platformLeadID    string    // backfilled on Verified transition

	createdAt              time.Time
	createdByMembershipID  MembershipID
	verifiedAt             time.Time
	verifiedByMembershipID MembershipID
	rejectedAt             time.Time
	rejectedByMembershipID MembershipID

	events []Event
}

// New constructs an UnverifiedContact in [StateNew] from a validated
// form and the originating Lead Agent's membership.
func New(id ID, form leadform.Form, createdBy MembershipID, now time.Time) (*UnverifiedContact, error) {
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("createdBy", createdBy.String()); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	c := &UnverifiedContact{
		id:                    id,
		form:                  form,
		state:                 StateNew,
		createdAt:             now,
		createdByMembershipID: createdBy,
	}
	c.recordEvent(CreatedEvent{
		ContactID:             id,
		CreatedAt:             now,
		CreatedByMembershipID: createdBy,
		MobileE164:            form.MobileE164(),
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
	ID                     ID
	Form                   leadform.Form
	State                  State
	RejectionReason        string
	BusyCallbackAt         time.Time
	BusyCallbackEndAt      time.Time
	PlatformLeadID         string
	CreatedAt              time.Time
	CreatedByMembershipID  MembershipID
	VerifiedAt             time.Time
	VerifiedByMembershipID MembershipID
	RejectedAt             time.Time
	RejectedByMembershipID MembershipID
}

// UnmarshalFromDB rehydrates a contact without re-validating (trusted-storage path).
func UnmarshalFromDB(s Snapshot) *UnverifiedContact {
	return &UnverifiedContact{
		id:                     s.ID,
		form:                   s.Form,
		state:                  s.State,
		rejectionReason:        s.RejectionReason,
		busyCallbackAt:         s.BusyCallbackAt,
		busyCallbackEndAt:      s.BusyCallbackEndAt,
		platformLeadID:         s.PlatformLeadID,
		createdAt:              s.CreatedAt,
		createdByMembershipID:  s.CreatedByMembershipID,
		verifiedAt:             s.VerifiedAt,
		verifiedByMembershipID: s.VerifiedByMembershipID,
		rejectedAt:             s.RejectedAt,
		rejectedByMembershipID: s.RejectedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the contact primary key.
func (c *UnverifiedContact) ID() ID { return c.id }

// Form returns the lead-form VO.
func (c *UnverifiedContact) Form() leadform.Form { return c.form }

// State returns the current lifecycle state.
func (c *UnverifiedContact) State() State { return c.state }

// RejectionReason returns the audit reason; empty unless rejected.
func (c *UnverifiedContact) RejectionReason() string { return c.rejectionReason }

// BusyCallbackAt returns the callback window start; zero unless [StateBusy].
func (c *UnverifiedContact) BusyCallbackAt() time.Time { return c.busyCallbackAt }

// BusyCallbackEndAt returns the callback window end; zero unless [StateBusy].
func (c *UnverifiedContact) BusyCallbackEndAt() time.Time { return c.busyCallbackEndAt }

// PlatformLeadID returns the projected PlatformLead ID; empty unless [StateVerified].
func (c *UnverifiedContact) PlatformLeadID() string { return c.platformLeadID }

// CreatedAt returns the row creation timestamp.
func (c *UnverifiedContact) CreatedAt() time.Time { return c.createdAt }

// CreatedByMembershipID returns the originating Lead Agent's membership.
func (c *UnverifiedContact) CreatedByMembershipID() MembershipID { return c.createdByMembershipID }

// VerifiedAt returns the verification timestamp; zero unless verified.
func (c *UnverifiedContact) VerifiedAt() time.Time { return c.verifiedAt }

// VerifiedByMembershipID returns the verifier's membership.
func (c *UnverifiedContact) VerifiedByMembershipID() MembershipID { return c.verifiedByMembershipID }

// RejectedAt returns the rejection timestamp; zero unless rejected.
func (c *UnverifiedContact) RejectedAt() time.Time { return c.rejectedAt }

// RejectedByMembershipID returns the rejector's membership.
func (c *UnverifiedContact) RejectedByMembershipID() MembershipID { return c.rejectedByMembershipID }

// ----- State transitions ----------------------------------------------------

// StartCall moves a New or Busy contact into InCall. Idempotent from
// InCall (no event); terminal states (Verified, Rejected) are rejected.
func (c *UnverifiedContact) StartCall(now time.Time) error {
	switch c.state {
	case StateInCall:
		return nil // already in call — idempotent
	case StateNew, StateBusy:
		// allowed
	case StateVerified:
		return fmt.Errorf("%w: cannot call a verified contact", ErrInvalid)
	case StateRejected:
		return fmt.Errorf("%w: cannot call a rejected contact", ErrInvalid)
	default:
		return fmt.Errorf("%w: invalid state %q", ErrInvalid, c.state)
	}
	c.state = StateInCall
	// Clear stale busy-window from a prior attempt.
	c.busyCallbackAt = time.Time{}
	c.busyCallbackEndAt = time.Time{}
	c.recordEvent(CallStartedEvent{
		ContactID: c.id,
		At:        now,
	})
	return nil
}

// MarkVerified moves an InCall contact to the Verified terminal state.
// The handler creates the PlatformLead in the same tx; this records the link.
func (c *UnverifiedContact) MarkVerified(platformLeadID string, verifiedBy MembershipID, now time.Time) error {
	if strings.TrimSpace(platformLeadID) == "" {
		return fmt.Errorf("%w: platformLeadID required", ErrInvalid)
	}
	if verifiedBy.IsZero() {
		return fmt.Errorf("%w: verifiedBy required", ErrInvalid)
	}
	if c.state == StateVerified {
		return nil // idempotent terminal
	}
	if c.state != StateInCall {
		return fmt.Errorf("%w: verify requires in_call state (got %q)", ErrInvalid, c.state)
	}
	c.state = StateVerified
	c.platformLeadID = platformLeadID
	c.verifiedAt = now
	c.verifiedByMembershipID = verifiedBy
	c.recordEvent(VerifiedEvent{
		ContactID:              c.id,
		PlatformLeadID:         platformLeadID,
		VerifiedAt:             now,
		VerifiedByMembershipID: verifiedBy,
	})
	return nil
}

// MarkRejected moves an InCall contact to the Rejected terminal state.
// reason is required for audit.
func (c *UnverifiedContact) MarkRejected(reason string, rejectedBy MembershipID, now time.Time) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: rejection reason required", ErrInvalid)
	}
	if rejectedBy.IsZero() {
		return fmt.Errorf("%w: rejectedBy required", ErrInvalid)
	}
	if c.state == StateRejected {
		return nil // idempotent terminal
	}
	if c.state != StateInCall {
		return fmt.Errorf("%w: reject requires in_call state (got %q)", ErrInvalid, c.state)
	}
	c.state = StateRejected
	c.rejectionReason = strings.TrimSpace(reason)
	c.rejectedAt = now
	c.rejectedByMembershipID = rejectedBy
	c.recordEvent(RejectedEvent{
		ContactID:              c.id,
		Reason:                 c.rejectionReason,
		RejectedAt:             now,
		RejectedByMembershipID: rejectedBy,
	})
	return nil
}

// MarkBusy moves an InCall contact to Busy with a callback window.
// Start must be future and end after start; caller provides now for determinism.
func (c *UnverifiedContact) MarkBusy(callbackAt, callbackEndAt time.Time, now time.Time) error {
	if callbackAt.IsZero() || callbackEndAt.IsZero() {
		return fmt.Errorf("%w: callback window required", ErrInvalid)
	}
	if !callbackAt.After(now) {
		return fmt.Errorf("%w: callback start must be in the future", ErrInvalid)
	}
	if !callbackEndAt.After(callbackAt) {
		return fmt.Errorf("%w: callback end must be after start", ErrInvalid)
	}
	if c.state != StateInCall {
		return fmt.Errorf("%w: busy requires in_call state (got %q)", ErrInvalid, c.state)
	}
	c.state = StateBusy
	c.busyCallbackAt = callbackAt
	c.busyCallbackEndAt = callbackEndAt
	c.recordEvent(MarkedBusyEvent{
		ContactID:     c.id,
		CallbackAt:    callbackAt,
		CallbackEndAt: callbackEndAt,
		At:            now,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains and returns recorded domain events. The repository
// calls it once per persist, in the same tx that writes state.
func (c *UnverifiedContact) PullEvents() []Event {
	if len(c.events) == 0 {
		return nil
	}
	out := c.events
	c.events = nil
	return out
}

func (c *UnverifiedContact) recordEvent(e Event) {
	c.events = append(c.events, e)
}

// Event is the sealed marker interface for unverified-contact domain events.
type Event interface{ isUnverifiedContactEvent() }

// CreatedEvent fires when [New] succeeds.
type CreatedEvent struct {
	ContactID             ID
	CreatedAt             time.Time
	CreatedByMembershipID MembershipID
	MobileE164            string
}

func (CreatedEvent) isUnverifiedContactEvent() {}

// CallStartedEvent fires on every [StartCall] transition, including Busy → InCall retry.
type CallStartedEvent struct {
	ContactID ID
	At        time.Time
}

func (CallStartedEvent) isUnverifiedContactEvent() {}

// VerifiedEvent fires on the InCall → Verified transition.
type VerifiedEvent struct {
	ContactID              ID
	PlatformLeadID         string
	VerifiedAt             time.Time
	VerifiedByMembershipID MembershipID
}

func (VerifiedEvent) isUnverifiedContactEvent() {}

// RejectedEvent fires on the InCall → Rejected transition.
type RejectedEvent struct {
	ContactID              ID
	Reason                 string
	RejectedAt             time.Time
	RejectedByMembershipID MembershipID
}

func (RejectedEvent) isUnverifiedContactEvent() {}

// MarkedBusyEvent fires on InCall → Busy. Notifications (Slice 2)
// subscribes to auto-create a Reminder for the originating Lead Agent.
type MarkedBusyEvent struct {
	ContactID     ID
	CallbackAt    time.Time
	CallbackEndAt time.Time
	At            time.Time
}

func (MarkedBusyEvent) isUnverifiedContactEvent() {}
