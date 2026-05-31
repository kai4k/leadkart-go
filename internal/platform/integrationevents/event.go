// Package integrationevents holds the Platform module's framework-neutral
// integration-event records and their domain→wire mapper.
//
// Mirrors internal/identity/integrationevents per ADR 0008 + 0046 + 0059.
// ZERO infrastructure deps — pure data records plus a marker interface. Wire
// serialisation lives in the outbox writer; transport selection in the forwarder.
//
// Event types end in V{N} per messaging.md "Event versioning": breaking changes
// ship a parallel V2 (never rename or drop a field on an existing version).
package integrationevents

import (
	"time"
)

// Topic is the canonical Watermill destination for all Platform integration
// events. The forwarder publishes here; subscribers route per [Event.Topic]
// alias via metadata header.
const Topic = "platform.events"

// Event is the marker interface every Platform integration-event record
// satisfies. Topic() returns the `platform.{event-kebab}.v{N}` alias;
// OccurredAt() the domain time of the event.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped tags events belonging to one tenant. Wire carries tenant_id as
// a UUID-shaped string (ADR 0059: cross-language consumers need no uuid codec,
// matches CRM's mirror byte-for-byte). The pipeline lifts it onto
// envelope.TenantId so subscribers see per-tenant scope.
//
// Each record file asserts: var _ TenantScoped = LeadPurchasedV1{}
type TenantScoped interface {
	Event
	TenantIDString() string
}

// Platform tags events with no tenant scope:
//
//   - UnverifiedContact, VerificationCall, PlatformLead-Verified (Platform tier).
//   - LeadCredit-init events (rare; not in Slice 1).
//   - System maintenance / impersonation audit (future).
//
// isPlatformEvent() is sealed: only events defined in this package can satisfy it.
type Platform interface {
	Event
	isPlatformEvent()
}

// platformMarker is embedded by every Platform record to satisfy the sealed
// interface without per-type redeclaration.
type platformMarker struct{}

func (platformMarker) isPlatformEvent() {}
