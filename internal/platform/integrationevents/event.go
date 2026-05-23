// Package integrationevents holds the Platform module's framework-
// neutral integration-event records + their domain→wire mapper.
//
// Mirror of internal/identity/integrationevents per ADR 0008 + 0046 +
// 0059. The package has ZERO infrastructure dependencies — pure data
// records + a marker interface. Wire serialisation lives in the
// outbox writer adapter; transport selection lives in the forwarder.
//
// All event types end in `V{N}` per `messaging.md` "Event versioning"
// canon — breaking changes ship a parallel V2 record (never rename or
// drop a field on an existing version).
package integrationevents

import (
	"time"
)

// Topic is the canonical Watermill destination for ALL Platform
// integration events. The platform outbox forwarder publishes here;
// subscribers (CRM in Phase 2.2, Notifications in Slice 2) consume +
// route per [Event.Topic] alias via metadata header.
const Topic = "platform.events"

// Event is the marker interface every Platform integration-event record
// satisfies. Topic() returns the alias `platform.{event-kebab}.v{N}`.
// OccurredAt() returns the domain-time at which the event happened.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped tags events that belong to ONE tenant. Wire shape
// carries `tenant_id` (UUID-shaped STRING per ADR 0059 frozen brief —
// cross-language consumers don't need a uuid codec, matches CRM
// subscriber's local mirror byte-for-byte). The runtime pipeline reads
// it onto Watermill's envelope.TenantId so subscribers see a per-
// tenant scope.
//
// Compile-time assertion shape in each record file:
//
//	var _ TenantScoped = LeadPurchasedV1{}
type TenantScoped interface {
	Event
	TenantIDString() string
}

// Platform tags events with NO tenant scope. Three classes apply:
//
//   - UnverifiedContact + VerificationCall + PlatformLead-Verified
//     (all live on the Platform tier; no tenant-context distinct from
//     the platform itself).
//   - LeadCredit-init events (rare; not in Slice 1).
//   - System maintenance / impersonation audit (future).
//
// The unexported isPlatformEvent() method is sealed within this
// package — only events DEFINED HERE can satisfy Platform.
type Platform interface {
	Event
	isPlatformEvent()
}

// platformMarker is embedded by every Platform record to satisfy the
// sealed interface without per-type method redeclaration.
type platformMarker struct{}

func (platformMarker) isPlatformEvent() {}
