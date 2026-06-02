// Package messaging wires the LeadKart Watermill messaging spine: the
// transactional outbox producer, the relay Forwarder, the subscriber
// Router + resilience middleware, and the cqrs typed dispatch on both
// sides. Cross-module communication is integration events on the bus,
// never direct handler edits. Choreography only — no sagas here.
//
// # Canonical stack (ADR 0067)
//
// Produce (in the aggregate's pgx.Tx): [PublishOutbox] drives a
// per-transaction [cqrs.EventBus] over a forwarder-decorated watermill-sql
// publisher, so the event row commits atomically with the aggregate write
// (outbox-first, ADR 0064). The producer and consumer share ONE
// [WireAliasMarshaler]: Name(event)=event.Topic() (the frozen wire alias,
// ADR 0059) carried in the event_type metadata header; payload is raw JSON.
// Encode and decode therefore cannot drift.
//
// Relay: a single Watermill [forwarder.Forwarder] drains the shared
// common.outbox queue table (watermill-sql v4 PostgreSQLQueueSchema, xid8
// ordering — ADR 0064) and republishes each event to its destination
// module topic, embedded in the envelope.
//
// Consume: a [cqrs.EventProcessor] ([NewEventProcessor]) hosts every
// module's typed handlers (cqrs.NewEventHandler[T]); GenerateSubscribeTopic
// derives the module topic from the event alias, and AckOnUnknownEvent is
// true because many event types ride one module topic.
//
// # Middleware stack (as wired in router.go)
//
//		global (outermost first):  CorrelationID → TraceContext → TenantContext
//		per-handler (outermost first, via AddSubscriber / AddCqrsHandler):
//		                           PoisonQueue → Idempotency → Audit → Retry → Recoverer
//
//	  - Recoverer is INNERMOST: a panicking handler becomes an error that
//	    Retry sees and retries (panics are no longer fatal-once).
//	  - Retry inside Audit: Audit records the final outcome once, after the
//	    retry budget is spent (or immediately for a [NonRetryable] error).
//	  - Idempotency inside PoisonQueue: the dedup row is written only on
//	    genuine success; a poisoned message is never marked "processed".
//	  - PoisonQueue OUTERMOST: after retries exhaust (or immediately for a
//	    [NonRetryable] error) it salvages the message to [DeadLetterTopic],
//	    persisted durably to common.dead_letter by [DeadLetterWriter] for
//	    inspection / replay. The DLQ persister carries ONLY Recoverer (a
//	    failed DLQ write must not re-poison into the same topic).
//	  - TraceContext extracts the producer's W3C trace context so the
//	    consumer span joins the producer trace across the async hop.
//	  - TenantContext bridges the tenant_id metadata header into ctx so
//	    tenant-scoped subscribers run under the right RLS scope.
//
// # Idempotency (inbox)
//
// [IdempotentReceiver] dedups per (message_id, handler_name) against
// identity.processed_messages. The contract is run-then-INSERT:
// at-least-once delivery + idempotent handlers — the TDL/Watermill canon
// (their CQRS guidance dedups in-handler; the Duplicator middleware exists
// to force handler idempotency). Every consumer handler is independently
// idempotent (business-key short-circuit or no-op-on-replay); the inbox
// row is a skip-redundant-work optimization, NOT the sole correctness
// mechanism. A transactional inbox (dedup + handler in one tx) was
// evaluated and deliberately NOT adopted — see ADR 0067 — because it adds
// no correctness over already-idempotent handlers and external-effect
// handlers (email / cache / SIEM) cannot be rolled back anyway.
package messaging
