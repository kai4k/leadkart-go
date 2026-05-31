package integrationevents

import (
	"time"
)

// VerificationCallLoggedV1 signals a Lead Agent recorded an outbound-call
// outcome. Platform-scoped; UUIDs are wire-shaped strings per ADR 0059. Slice 2
// subscribers project onto dashboards and auto-create Reminders for Busy outcomes.
type VerificationCallLoggedV1 struct {
	platformMarker

	CallID               string    `json:"call_id"`
	ContactID            string    `json:"contact_id"`
	OutcomeCode          string    `json:"outcome_code"`
	LoggedAt             time.Time `json:"logged_at"`
	LoggedByMembershipID string    `json:"logged_by_membership_id"`
}

// Topic returns the wire alias.
func (VerificationCallLoggedV1) Topic() string { return "platform.verification_call_logged.v1" }

// OccurredAt returns the domain timestamp.
func (e VerificationCallLoggedV1) OccurredAt() time.Time { return e.LoggedAt }

var (
	_ Platform = VerificationCallLoggedV1{}
	_          = register(VerificationCallLoggedV1{})
)
