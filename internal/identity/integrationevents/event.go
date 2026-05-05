package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL Identity
// integration events. The OutboxForwarder publishes here; subscriber
// handlers (in-module + future cross-module modules) consume from here
// and route per [Event.Topic] alias via metadata header.
//
// Single-topic-per-source-module pattern aligns with Wolverine canon
// + watermill-sql forwarder defaults.
const Topic = "identity.events"

// Event is the marker interface every Identity integration-event record
// satisfies. Carries the canonical wire-alias (`Topic`) + the
// domain-time of the event (`OccurredAt`).
//
// `Topic` returns the alias `identity.{event-kebab}.v{N}` per
// `messaging.md` "Event versioning". Outbox + Watermill envelope
// metadata route on this string.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant. The
// Wolverine-equivalent `Envelope.TenantId` is populated from
// [TenantID] + the canonical channel + the per-tenant fanout
// subscriber model per `messaging.md` "Tenant channel".
//
// Compile-time assertion in each file:
//
//	var _ TenantScoped = MembershipCreatedV1{}
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}

// Platform is the marker for events with NO tenant scope. Three classes
// of event qualify:
//
//   - Tenant-aggregate events (registration, suspension, deletion) —
//     the tenant itself is the data; there is no "current tenant"
//     context distinct from the event's tenant_id field.
//   - Person-aggregate events (creation, anonymisation, password
//     change) — Person is global identity; the .NET side classifies
//     these as `[PlatformEvent]` per `messaging.md`.
//   - Genuine platform-operator events — system maintenance,
//     impersonation audit, etc.
//
// The unexported `isPlatformEvent` method is sealed within this
// package — only events DEFINED HERE can satisfy Platform. Compile-time
// assertion in each file:
//
//	var _ Platform = TenantRegisteredV1{}
type Platform interface {
	Event
	isPlatformEvent()
}

// platformMarker is embedded by every Platform record to satisfy the
// sealed interface without each type re-defining the method.
type platformMarker struct{}

func (platformMarker) isPlatformEvent() {}
