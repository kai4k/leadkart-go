// Package dispatch is the Dispatch bounded context — consignment notes
// + carrier-status tracking. Per BRD §6.6.
//
// STATUS: scaffold, NOT wired. The domain, app, ports/subscribers, and
// integrationevents packages exist, but there is no adapters/ layer
// (no pg repository, no outbox, no forwarder) and ports/subscribers'
// Register is not called from any cmd/ composition root. So nothing in
// this module runs yet. The OrderPacked→ConsignmentNote flow below is
// the intended design, not live behaviour. It also depends on the
// Orders module publishing orders.order_packed.v1, which Orders does
// not yet do (Orders is domain-only).
//
// Intended layout:
//
//	internal/dispatch/
//	├── domain/                 ConsignmentNote aggregate              [present]
//	├── app/                    command + query handlers              [present]
//	├── ports/                  HTTP + subscribers (OrderPacked → note) [present, unwired]
//	├── adapters/               pgx/sqlc + outbox                      [MISSING]
//	└── integrationevents/      framework-neutral wire records        [present]
//
// Owner of:
//
//   - ConsignmentNote (formal: "consignment note"; BRD §4.8 informal:
//     "builty"). Transport document handed to carrier on dispatch.
//
// Lifecycle (BRD §6.6):
//
//	pending → dispatched → in_transit → delivered | failed
//
// Per ADR 0063 §4 the Dispatch module participates in the order
// fulfillment saga as one of the subscribers — consumes
// `orders.order_packed.v1` to create a ConsignmentNote slot;
// publishes `dispatch.consignment_delivered.v1` to signal the order
// for delivery transition.
//
// Carrier status webhook receives external delivery updates + updates
// status. v0.2 ships with a single carrier-agnostic webhook +
// generic-shape payload; per-carrier adapters (BlueDart / DTDC /
// India Post APIs) land in v0.3+.
package dispatch
