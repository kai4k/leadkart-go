package refreshtoken

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for refresh-token-family
// domain events. Sealed via the unexported isFamilyEvent() method so
// only types in this package can satisfy it — same shape as
// role.Event.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// integrationevents.*V1 per Vernon IDDD ch. 8 ("Domain Events vs.
// Integration Events"): a v2 wire rename must NOT force a domain edit.
// The integration mapper in internal/identity/integrationevents/
// type-switches on these structs and emits the canonical V1 envelope.
type Event interface {
	isFamilyEvent()
}

// FamilyCreatedEvent fires when a new family is issued via [NewFamily].
type FamilyCreatedEvent struct {
	FamilyID    FamilyID
	PersonID    person.ID
	TenantID    tenant.ID
	DeviceLabel string
	At          time.Time
}

func (FamilyCreatedEvent) isFamilyEvent() {}

// RotatedEvent fires on successful rotation. Carries PersonID + TenantID
// so downstream subscribers can react without calling back to the
// publisher (Udi Dahan event-autonomy rule per messaging.md).
type RotatedEvent struct {
	FamilyID           FamilyID
	PersonID           person.ID
	TenantID           tenant.ID
	ConsumedTokenID    TokenID
	NewTokenID         TokenID
	NewTokenGeneration int32
	At                 time.Time
}

func (RotatedEvent) isFamilyEvent() {}

// RevokedEvent fires when a family is revoked — by [Revoke], by reuse
// detection in [Rotate], or by family-cap eviction. Reason disambiguates;
// "reuse_detected" is the security-incident path Notifications subscribes
// to for SIEM alerts.
//
// Carries PersonID + TenantID for the same Udi Dahan reason as RotatedEvent.
type RevokedEvent struct {
	FamilyID FamilyID
	PersonID person.ID
	TenantID tenant.ID
	Reason   string
	At       time.Time
}

func (RevokedEvent) isFamilyEvent() {}
