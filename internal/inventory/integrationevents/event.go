// Package integrationevents holds the Inventory module's wire-stable
// integration-event catalogue + the domain↔V1 mapping. Mirror of
// internal/identity/integrationevents/ — Unobtrusive Mode (framework-
// neutral records; no Watermill / pgx / jwt imports allowed per the
// arch test).
//
// Per ADR 0008 + messaging.md "Event versioning": every event carries
// a canonical alias `inventory.{event-kebab}.v{N}` returned by Topic().
// Cross-module consumers (Orders, Dispatch, Notifications) subscribe
// on these topics; payload changes ship as a new vN+1 record without
// renaming or mutating the existing version.
package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL Inventory
// integration events. The forwarder publishes here; subscriber handlers
// route per [Event.Topic] alias via metadata header — same single-topic-
// per-module pattern as the Identity module + Wolverine canon.
const Topic = "inventory.events"

// Event is the marker interface every Inventory integration-event
// record satisfies.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant.
// Every Inventory event is tenant-scoped at v0.2 (the Product aggregate
// is tenant-scoped; Platform-tier inventory metadata isn't part of
// slice 1).
//
// Compile-time assertion in each file:
//
//	var _ TenantScoped = ProductCreatedV1{}
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}
