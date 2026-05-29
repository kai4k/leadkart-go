// Package notifications is the Notifications bounded context — cross-
// module user-facing alerts (in-app inbox + push + future email digest).
// Per BRD §6.9 + Udi Dahan's "subscriber-decides" pattern.
//
// STATUS: domain-only skeleton. Only domain/notification exists; there is
// no app/, ports/, or adapters/ layer yet and nothing is wired into a
// cmd/ host. The layout below is the target, not the current state.
//
// Target layout:
//
//	internal/notifications/
//	├── domain/notification/           the Notification aggregate
//	├── app/                           command + query handlers
//	├── ports/                         HTTP (inbox) + subscribers (cross-module)
//	├── adapters/                      pgx/sqlc
//	└── integrationevents/             outbound events (read receipts etc)
//
// Architectural shape per BRD §6.9 ("Subscriber-decides pattern"):
//
//   - Notifications module SUBSCRIBES directly to other modules'
//     integration events (LeadAssignedV1, OrderConfirmedV1,
//     WorkItemOverdueV1, …) + decides recipient + content. Publishers
//     do NOT know about notifications.
//   - Per-recipient dedup window (5 min) on
//     `(recipient_membership_id, source_entity_type, source_entity_id,
//     category)` — at-least-once delivery means producers can replay
//     events; dedup makes that safe.
//   - Read-mostly storage — Notification rows are written once + read
//     many. Purge cron: read after 7 days, unread after 30 days.
//   - Real-time push via coder/websocket (ADR 0016) when the recipient
//     has an active connection; fall back to next-fetch on reconnect.
//
// Deviation from .NET parent: the parent uses Marten document store +
// SignalR. Go uses Postgres + coder/websocket per the project's tech
// stack picks (ADRs 0004, 0016). Subscriber-decides pattern is
// identical.
package notifications
