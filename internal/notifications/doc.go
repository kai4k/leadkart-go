// Package notifications is the Notifications bounded context — cross-module
// user-facing alerts (in-app inbox + push + future email digest). BRD §6.9,
// Udi Dahan's subscriber-decides pattern.
//
// STATUS: domain-only skeleton. Only domain/notification exists; no app/,
// ports/, or adapters/ yet, and nothing is wired into a cmd/ host. The layout
// below is the target, not the current state.
//
//	internal/notifications/
//	├── domain/notification/           the Notification aggregate
//	├── app/                           command + query handlers
//	├── ports/                         HTTP (inbox) + subscribers (cross-module)
//	├── adapters/                      pgx/sqlc
//	└── integrationevents/             outbound events (read receipts etc)
//
// Shape (BRD §6.9, subscriber-decides):
//
//   - This module subscribes directly to other modules' integration events
//     (LeadAssignedV1, OrderConfirmedV1, WorkItemOverdueV1, …) and decides
//     recipient + content. Publishers don't know about notifications.
//   - 5-min per-recipient dedup on (recipient_membership_id, source_entity_type,
//     source_entity_id, category): at-least-once delivery lets producers replay;
//     dedup makes replay safe.
//   - Read-mostly storage — rows written once, read many. Purge cron: read after
//     7 days, unread after 30.
//   - Real-time push via coder/websocket (ADR 0016) when the recipient is
//     connected; else next-fetch on reconnect.
//
// Deviation from .NET parent: parent uses Marten + SignalR; Go uses Postgres +
// coder/websocket per stack picks (ADR 0004, 0016). Pattern is identical.
package notifications
