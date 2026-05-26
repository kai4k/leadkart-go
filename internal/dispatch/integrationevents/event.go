// Package integrationevents holds the framework-neutral Dispatch
// integration-event vocabulary per ADR 0008 + 0063 §4. Records here
// are the wire-stable shape — `dispatch.{event-kebab}.v{N}` —
// published to the Watermill `dispatch.events` topic by the outbox
// forwarder.
//
// Dispatch publishes:
//
//   - dispatch.consignment_note_created.v1   — slot born on OrderPacked
//   - dispatch.consignment_note_dispatched.v1 — goods left warehouse
//   - dispatch.consignment_note_in_transit.v1 — carrier in-transit scan
//   - dispatch.consignment_delivered.v1      — terminal-success; ADR 0063
//     §4 saga input — Orders subscriber transitions Order → delivered.
//   - dispatch.consignment_note_failed.v1    — terminal-failure
//
// Dispatch consumes (subscriber side; payloads are LOCAL MIRRORS until
// the Orders module's integrationevents package lands on the same
// branch; the modular-monolith carve-out per ADR 0001 permits importing
// the publisher's integrationevents package once both branches merge):
//
//   - orders.order_packed.v1   — drives ConsignmentNote slot creation
//   - orders.order_cancelled.v1 — drives the cancel-consignment compensation
package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL Dispatch
// integration events. Mirror of identity.events / crm.events / etc.
const Topic = "dispatch.events"

// Event is the marker interface every Dispatch integration-event
// record satisfies. Carries the canonical wire-alias + the domain-time
// of the event.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant.
// Every Dispatch event is TenantScoped — Dispatch has no cross-tenant ops.
//
// Compile-time assertion shape per file:
//
//	var _ TenantScoped = ConsignmentNoteCreatedV1{}
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}
