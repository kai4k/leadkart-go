// Package integrationevents holds the framework-neutral CRM
// integration-event vocabulary per ADR 0008 + 0060. Records here are
// the wire-stable shape — `crm.{event-kebab}.v{N}` — published to the
// Watermill `crm.events` topic by the outbox forwarder.
//
// CRM consumes integration events from sibling modules (Platform's
// `platform.lead-purchased.v1`; Identity's `identity.tenant_suspended.v1`,
// `identity.membership_deactivated.v1`) via the subscriber layer — the
// CONSUMED-event records live in those modules' own integrationevents
// packages and are imported when needed. This package owns ONLY the
// CRM-emitted vocabulary.
package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL CRM integration
// events. The CRM OutboxForwarder publishes here; subscriber handlers
// (in-module + future cross-module modules) consume from here and route
// per [Event.Topic] alias via the metadata header.
//
// Single-topic-per-source-module pattern aligns with the Wolverine
// canon + watermill-sql forwarder defaults; mirror of identity.events.
const Topic = "crm.events"

// Event is the marker interface every CRM integration-event record
// satisfies. Carries the canonical wire-alias (`Topic`) + the
// domain-time of the event (`OccurredAt`).
//
// `Topic` returns the alias `crm.{event-kebab}.v{N}` per messaging.md
// "Event versioning". Outbox + Watermill envelope metadata route on
// this string.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant.
// Every CRM event is TenantScoped — CRM has no cross-tenant ops.
//
// Compile-time assertion in each file:
//
//	var _ TenantScoped = CrmLeadCreatedV1{}
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}
