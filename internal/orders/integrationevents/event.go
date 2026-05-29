// Package integrationevents holds the Orders module's framework-neutral
// integration-event records — the wire-stable vocabulary the Orders
// outbox forwarder publishes to the Watermill `orders.events` topic.
//
// Per ADR 0001 + 0008 + 0063: this is the anti-corruption-layer boundary
// (Vernon IDDD ch. 13). Sibling modules (Dispatch, Notifications) import
// THIS package's records to decode the envelopes they consume — they
// NEVER reach into orders/domain, orders/app, orders/ports, or
// orders/adapters. The package carries ZERO infrastructure dependencies:
// pure data records + marker interfaces.
//
// STATUS: the Orders aggregates that EMIT these events are a pending
// scaffold (see internal/orders/doc.go — domain-only skeleton, no
// producer wired yet). The wire contract is the integration boundary
// and legitimately exists before the producer internals: a consumer
// (Dispatch) already depends on the shape, so the contract is the
// source of truth both sides agree on. Mirror of
// internal/platform/integrationevents.
package integrationevents

import (
	"time"
)

// Topic is the canonical Watermill destination for ALL Orders
// integration events. The Orders OutboxForwarder publishes here;
// subscribers (Dispatch in the fulfillment saga, Notifications in
// Slice 2) consume + route per [Event.Topic] alias via the metadata
// header. Single-topic-per-source-module pattern — mirror of
// platform.events / crm.events.
const Topic = "orders.events"

// Event is the marker interface every Orders integration-event record
// satisfies. Topic() returns the alias `orders.{event-kebab}.v{N}`;
// OccurredAt() returns the domain-time at which the event happened.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped tags events that belong to ONE tenant. Wire shape carries
// `tenant_id` (UUID-shaped STRING per ADR 0059 frozen brief —
// cross-language consumers don't need a uuid codec, matches the Dispatch
// subscriber's decode shape byte-for-byte). The runtime pipeline reads
// it onto Watermill's envelope.TenantId so subscribers see a per-tenant
// scope.
//
// Compile-time assertion shape in each record file:
//
//	var _ TenantScoped = OrderPackedV1{}
type TenantScoped interface {
	Event
	TenantIDString() string
}

// Platform tags events with NO tenant scope. The unexported
// isPlatformEvent() method is sealed within this package — only events
// DEFINED HERE can satisfy Platform. (No Orders event is platform-scoped
// today; the marker exists for shape-parity with the sibling modules so
// the cross-cutting arch tests apply uniformly.)
type Platform interface {
	Event
	isPlatformEvent()
}

// platformMarker is embedded by every Platform record to satisfy the
// sealed interface without per-type method redeclaration.
type platformMarker struct{}

func (platformMarker) isPlatformEvent() {}
