// Package messaging wires the LeadKart Watermill router and its
// middleware stack. Cross-module communication is integration events on
// the bus, never direct handler edits.
//
// The actual stack (as wired in router.go) is two layers:
//
//	global:       Recoverer → TraceContext → CorrelationID → TenantContext
//	per-handler:  Idempotency → Audit → Retry → handler
//
//   - Recoverer converts a panicking handler into an error. NOTE: it is
//     currently outermost, so a recovered panic does NOT reach Retry —
//     panicking handlers don't retry. There is also no PoisonQueue/DLQ
//     yet, so a handler that keeps returning an error (e.g. on a
//     permanently malformed payload) retries indefinitely. Both are
//     known gaps; the fix (Recoverer-inside-Retry + PoisonQueue for
//     non-retryable errors) is tracked for the subscriber-resilience
//     pass. Do not claim DLQ behaviour until it ships.
//   - TraceContext extracts the W3C trace context from message metadata
//     so the consumer span joins the producer's trace.
//   - CorrelationID propagates the correlation chain across the async hop.
//   - TenantContext bridges the tenant_id metadata header into ctx via
//     tenancy.WithID so tenant-scoped subscribers run under the right RLS
//     scope.
//   - Idempotency wraps the handler in a (message_id, handler_name) dedup
//     check (run-then-insert; not yet transactional with the handler's
//     side effects). Backstop for at-least-once delivery.
//   - Audit auto-writes a row to buildingblocks.audit_log_entry per
//     processed message (success or failure).
//   - Retry retries transient handler errors under the same message +
//     correlation chain.
//
// Choreography only — no sagas in this package.
package messaging
