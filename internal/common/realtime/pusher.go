// Package realtime is the cross-module real-time push abstraction.
// Per BRD §6.10 + ADR 0016 (coder/websocket + SSE for the v0.2 path).
//
// Modules emit pushes via the [Pusher] interface WITHOUT knowing what
// transport carries them. The composition root wires a concrete
// [Pusher] (WebsocketPusher / NoopPusher for tests / multi-pusher for
// fan-out across transports). Notifications module is the heaviest
// consumer per BRD §6.9 — every CreatedEvent → Pusher.PushToMembership.
//
// The shape mirrors the .NET parent's IRealTimePusher contract:
//
//	PushToMembershipAsync(membershipID, eventName, payload)
//	PushToTenantAsync(tenantID, eventName, payload)
//
// Fire-and-forget semantics — disconnected clients are EXPECTED; the
// persisted source (notification row, work item, etc) is the source
// of truth on next-fetch + the badge-count poll. A failed push is
// LOGGED but never errors back to the caller (per BRD §6.10
// "backpressure: SendAsync is fire-and-forget").
package realtime

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// EventName is the wire-stable identifier the client routes on.
// Conventionally `<module>.<event-kebab>.v{N}` mirroring the
// integration-event topic alias — same string used in the WebSocket
// envelope's `event` field.
type EventName string

// Envelope is the payload shape pushed to subscribers. Generic JSON-
// shaped data — the client's renderer routes by EventName + decodes
// Data into the corresponding type.
type Envelope struct {
	Event EventName `json:"event"`
	Data  any       `json:"data"`
}

// Pusher fans out real-time deliveries. Implementations:
//
//   - WebsocketPusher (v0.2) — coder/websocket over an authenticated
//     /ws endpoint; tracks (membershipID, tenantID) → connection map.
//   - NoopPusher — disabled in test fixtures + the headless worker.
//   - MultiPusher — combinator for the v0.3 SSE-fallback path.
//
// Methods never return errors: per BRD §6.10 backpressure model,
// disconnected = lost-this-cycle (state persists; client re-syncs on
// reconnect). Implementations LOG failures via slog but don't surface
// them to the caller.
type Pusher interface {
	// PushToMembership delivers env to every active connection bound
	// to the supplied (tenantID, membershipID). Zero connections is a
	// no-op (the client will fetch on next page load).
	PushToMembership(ctx context.Context, tenantID tenant.ID, membershipID membership.ID, env Envelope)

	// PushToTenant delivers env to every active connection in the
	// tenant — used for tenant-wide broadcasts (UserHierarchyCascaded,
	// TenantSettingsUpdated, etc per BRD §6.9). Notifications module
	// uses the per-membership variant; cross-cutting modules use this.
	PushToTenant(ctx context.Context, tenantID tenant.ID, env Envelope)
}
