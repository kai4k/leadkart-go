// Package dispatch is the Dispatch bounded context — consignment notes
// + carrier-status tracking. Per BRD §6.6.
//
// Layout per CLAUDE.md "Three unbreakable rules":
//
//	internal/dispatch/
//	├── domain/                 ConsignmentNote aggregate
//	├── app/                    command + query handlers
//	├── ports/                  HTTP + subscribers (OrderPacked → create note)
//	├── adapters/               pgx/sqlc + outbox
//	└── integrationevents/      framework-neutral wire records
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
