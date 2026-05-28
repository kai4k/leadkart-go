// Package subscribers holds Dispatch-side Watermill subscriber
// handlers. Per ADR 0001 modular-monolith canon — cross-module
// communication is via integration events on the bus.
//
// Subscribers in this package:
//
//   - [OrderPackedIngestor] — subscribes to `orders.events` topic,
//     filters by `orders.order_packed.v1`, creates a pending
//     ConsignmentNote slot via [command.CreateConsignmentNoteHandler].
//     Per ADR 0063 §4 fulfillment-saga.
//
// Idempotency: every subscriber is idempotent. The
// `messaging.IdempotentReceiver` middleware (envelope-ID dedup) is the
// first line; the natural-key precheck inside the command (one
// ConsignmentNote per Order — partial unique index) is the backstop.
//
// CROSS-MODULE EVENT MIRROR: until the Orders module's
// integrationevents package lands on the same branch, the
// [OrderPackedV1] struct in this package is a LOCAL MIRROR of the
// canonical `orders.order_packed.v1` wire contract. Wire payload
// (JSON) is identical so consumers don't need rewiring when the mirror
// is later replaced with an import. Per the precedent in
// internal/crm/ports/subscribers/lead_purchased_payload.go.
package subscribers
