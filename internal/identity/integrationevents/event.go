package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for all Identity integration
// events. The OutboxForwarder publishes here; subscribers route per
// [Event.Topic] alias via the metadata header.
// Single-topic-per-source-module pattern follows Wolverine canon.
const Topic = "identity.events"

// Event is the marker interface every Identity integration-event record
// satisfies. Topic returns the wire alias `identity.{event-kebab}.v{N}`;
// outbox + Watermill envelope metadata route on this string.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped marks events that belong to exactly one tenant.
// Per-file compile-time assertion:
//
//	var _ TenantScoped = MembershipCreatedV1{}
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}

// Platform marks events with no per-tenant scope: tenant-aggregate events
// (registration, suspension, deletion), global Person events, and
// platform-operator events. The unexported isPlatformEvent method seals
// the interface — only types in this package can satisfy it.
// Per-file compile-time assertion:
//
//	var _ Platform = TenantRegisteredV1{}
type Platform interface {
	Event
	isPlatformEvent()
}

// platformMarker is embedded by Platform records to satisfy the sealed interface.
type platformMarker struct{}

func (platformMarker) isPlatformEvent() {}
