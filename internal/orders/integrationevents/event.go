// Package integrationevents holds the Orders module's framework-neutral
// integration-event records — wire-stable vocabulary published to the
// Watermill `orders.events` topic (ADR 0001 + 0008 + 0063).
//
// ACL boundary (Vernon IDDD ch. 13): sibling modules import this package
// to decode consumed envelopes; they never reach into orders/domain,
// orders/app, orders/ports, or orders/adapters. Zero infrastructure
// dependencies — pure data records + marker interfaces.
//
// Mirror of internal/platform/integrationevents.
package integrationevents

import (
	"time"
)

// Topic is the Watermill destination for all Orders integration events.
// Subscribers route per [Event.Topic] alias via the metadata header.
const Topic = "orders.events"

// Event is the marker interface every Orders integration-event record satisfies.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped tags events belonging to a single tenant. Wire field
// `tenant_id` is a UUID-as-string (ADR 0059). The pipeline promotes it
// to Watermill's envelope.TenantId for per-tenant subscriber routing.
//
// Compile-time assertion shape in each record file:
//
//	var _ TenantScoped = OrderPackedV1{}
type TenantScoped interface {
	Event
	TenantIDString() string
}
