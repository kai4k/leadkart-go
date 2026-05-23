package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// VerificationCallLoggedV1 — a Lead Agent recorded the outcome of an
// outbound call. Platform-scoped. Subscribers in Slice 2 will project
// onto operator dashboards + auto-create Reminders for Busy outcomes.
type VerificationCallLoggedV1 struct {
	platformMarker

	CallID               uuid.UUID `json:"call_id"`
	ContactID            uuid.UUID `json:"contact_id"`
	OutcomeCode          string    `json:"outcome_code"`
	LoggedAt             time.Time `json:"logged_at"`
	LoggedByMembershipID uuid.UUID `json:"logged_by_membership_id"`
}

// Topic returns the canonical wire alias.
func (VerificationCallLoggedV1) Topic() string { return "platform.verification_call_logged.v1" }

// OccurredAt returns the domain timestamp.
func (e VerificationCallLoggedV1) OccurredAt() time.Time { return e.LoggedAt }

var (
	_ Platform = VerificationCallLoggedV1{}
	_          = register(VerificationCallLoggedV1{})
)
