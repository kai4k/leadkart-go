package integrationevents

import (
	"time"
)

// UnverifiedContactCreatedV1 signals a Lead Agent registered a raw contact in
// the Platform work queue. Platform-scoped. UUIDs are wire-shaped strings per
// ADR 0059. No Slice 1 consumer (reserved for Slice 2 analytics/audit).
type UnverifiedContactCreatedV1 struct {
	platformMarker

	ContactID             string    `json:"contact_id"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedByMembershipID string    `json:"created_by_membership_id"`
	MobileE164            string    `json:"mobile_e164"`
}

// Topic returns the wire alias.
func (UnverifiedContactCreatedV1) Topic() string { return "platform.unverified_contact_created.v1" }

// OccurredAt returns the domain timestamp.
func (e UnverifiedContactCreatedV1) OccurredAt() time.Time { return e.CreatedAt }

var (
	_ Platform = UnverifiedContactCreatedV1{}
	_          = register(UnverifiedContactCreatedV1{})
)
