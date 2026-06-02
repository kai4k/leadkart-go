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
// CROSS-MODULE EVENT CONTRACT: the `orders.order_packed.v1` wire shape
// is owned by the producer side — [ordersevents.OrderPackedV1] in
// internal/orders/integrationevents. This package IMPORTS it (the
// sanctioned anti-corruption-layer path, per the precedent in
// internal/crm/ports/subscribers/lead_purchased_payload.go) rather than
// mirroring the struct locally, so producer and consumer cannot drift.
package subscribers
