// Package integrationevents holds the framework-neutral companysettings
// integration-event vocabulary (ADR 0008, ADR 0070). Wire-stable shape:
// `companysettings.{event}.v{N}`, published to the `companysettings.events`
// topic by the shared outbox forwarder.
//
// companysettings publishes:
//
//   - companysettings.policy_changed.v1 — a tenant's policy moved to a new
//     version, or the daily job re-announced the current one. Carries the whole
//     policy plus its version: Orders and Inventory upsert it WHERE the version
//     is newer, so a duplicate or reordered event is harmless.
//
// companysettings consumes identity.tenant_registered.v1 (seed version 1).
package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Topic is the canonical Watermill destination for ALL companysettings
// integration events.
const Topic = "companysettings.events"

// Event is the marker interface every companysettings integration-event
// record satisfies.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// TenantScoped is the marker for events that belong to ONE tenant. Every
// companysettings event is TenantScoped.
type TenantScoped interface {
	Event
	TenantID() uuid.UUID
}
