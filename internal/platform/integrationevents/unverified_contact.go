package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// UnverifiedContactCreatedV1 — a Lead Agent registered a new raw
// contact in the Platform work queue. Platform-scoped (no tenant).
//
// Slice 1 consumers: none (reserved for future analytics + audit
// projection in Slice 2). Wire shape frozen here per ADR 0059.
type UnverifiedContactCreatedV1 struct {
	platformMarker

	ContactID             uuid.UUID `json:"contact_id"`
	CreatedAt             time.Time `json:"created_at"`
	CreatedByMembershipID uuid.UUID `json:"created_by_membership_id"`
	MobileE164            string    `json:"mobile_e164"`
}

// Topic returns the canonical wire alias.
func (UnverifiedContactCreatedV1) Topic() string { return "platform.unverified_contact_created.v1" }

// OccurredAt returns the domain timestamp.
func (e UnverifiedContactCreatedV1) OccurredAt() time.Time { return e.CreatedAt }

// Compile-time + registry assertions.
var (
	_ Platform = UnverifiedContactCreatedV1{}
	_          = register(UnverifiedContactCreatedV1{})
)
