// Package messaging wires the LeadKart Watermill router + the
// canonical middleware stack per `messaging.md` doctrine + `architecture.md`
// "Cross-module extensibility — integration events, not handler edits".
//
// Stack composition order (outer-most → inner-most), per messaging.md:
//
//	Recoverer → CorrelationID → TenantContext → Idempotency → Audit → Retry → handler
//
//   - Recoverer turns a panicking handler into an error so the broker
//     can DLQ + the process keeps serving.
//   - CorrelationID propagates the X-Correlation-Id chain so a single
//     trace spans HTTP → outbox → forwarder → subscriber.
//   - TenantContext bridges Envelope.TenantId metadata into ctx via
//     `tenancy.WithID`; subscribers run with the correct tenant scope
//     for downstream RLS-bound queries (parallels .NET
//     TenantContextMiddleware in messaging.md).
//   - Idempotency wraps the handler in a (message_id, handler_name)
//     dedup table lookup against `identity.processed_messages` —
//     replay-safe at-least-once delivery (Layer 2 of messaging.md
//     "Idempotency").
//   - Audit auto-writes a row to buildingblocks.audit_log_entry for
//     every processed message (success OR failure).
//   - Retry sits innermost so a transient error inside the handler
//     gets retried under the SAME (message_id, handler_name) +
//     SAME correlation chain.
//
// Sagas are NOT in this package per TDL canon (plan §G.H.4 + ADR
// 0031). Choreography only.
//
// Citations: ThreeDotsLabs Watermill v1.5 docs; messaging.md doctrine;
// Wolverine middleware-pipeline parallel.
package messaging
