// Package integrationevents holds the framework-neutral Tasks
// integration-event vocabulary per ADR 0008 + BRD §6.8. Records here
// are the wire-stable shape — `tasks.{event-kebab}.v{N}` — published
// to the Watermill `tasks.events` topic by the outbox forwarder.
//
// Tasks consumes integration events from sibling modules (CRM's
// `crm.call_logged.v1`, `crm.lead_converted.v1`; future Orders /
// Inventory events) via the subscriber layer — the CONSUMED-event
// records live in those modules' own integrationevents packages (or
// in local mirror payload files when those modules haven't shipped
// yet). This package owns ONLY the Tasks-emitted vocabulary.
package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL Tasks
// integration events. The Tasks OutboxForwarder publishes here;
// subscriber handlers (in-module + future cross-module) consume from
// here and route per [Event.Topic] alias via the metadata header.
//
// Single-topic-per-source-module pattern aligns with the Wolverine
// canon + watermill-sql forwarder defaults; mirror of crm.events +
// identity.events + platform.events.
const Topic = "tasks.events"

// Event is the marker interface every Tasks integration-event record
// satisfies. Carries the canonical wire-alias (`Topic`) + the
// domain-time of the event (`OccurredAt`).
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant.
// Every Tasks event is TenantScoped — Tasks has no cross-tenant ops.
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}
