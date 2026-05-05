package refreshtoken

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the marker interface for refresh-token-family domain events.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// FamilyCreatedEvent fires when a new family is issued via [NewFamily].
type FamilyCreatedEvent struct {
	FamilyID    FamilyID
	PersonID    person.ID
	TenantID    tenant.ID
	DeviceLabel string
	At          time.Time
}

// Topic returns the integration-event type.
func (FamilyCreatedEvent) Topic() string { return "identity.refresh_token_family_created.v1" }

// OccurredAt returns the domain timestamp.
func (e FamilyCreatedEvent) OccurredAt() time.Time { return e.At }

// RotatedEvent fires on successful rotation.
type RotatedEvent struct {
	FamilyID           FamilyID
	ConsumedTokenID    TokenID
	NewTokenID         TokenID
	NewTokenGeneration int32
	At                 time.Time
}

// Topic returns the integration-event type.
func (RotatedEvent) Topic() string { return "identity.refresh_token_rotated.v1" }

// OccurredAt returns the domain timestamp.
func (e RotatedEvent) OccurredAt() time.Time { return e.At }

// RevokedEvent fires when a family is revoked — by [Revoke], by reuse
// detection in [Rotate], or by family-cap eviction in the application
// service. The Reason field disambiguates the trigger.
//
// Subscribers (Notifications module) react to "reuse_detected" with a
// SIEM alert + user-notification email — that's a security incident.
type RevokedEvent struct {
	FamilyID FamilyID
	Reason   string
	At       time.Time
}

// Topic returns the integration-event type.
func (RevokedEvent) Topic() string { return "identity.refresh_token_family_revoked.v1" }

// OccurredAt returns the domain timestamp.
func (e RevokedEvent) OccurredAt() time.Time { return e.At }
