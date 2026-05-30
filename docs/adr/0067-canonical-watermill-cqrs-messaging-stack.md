# ADR 0067 — Canonical Watermill messaging stack: cqrs component on both sides, wire-alias marshaler, resilience, durable DLQ

**Status:** Accepted
**Date:** 2026-05-31
**Completes:** [ADR 0064](0064-outbox-as-relay-and-watermill-forwarder.md) (Forwarder + watermill-sql adoption).
**Supersedes (in part):** [ADR 0008](0008-messaging-watermill.md) (hand-rolled event routing), [ADR 0009](0009-async-command-event-handling.md) (the event-routing portion; commands stay synchronous — see Decision 5).
**Builds on:** [ADR 0056](0056-rfc8693-act-claim-propagation.md) (act-claim metadata), [ADR 0059](0059-frozen-integration-event-wire-contract.md) (frozen wire alias).

## Context

ADR 0064 mandated Watermill's `Forwarder` + `watermill-sql` for the relay but left the rest of the spine hand-rolled: a bespoke event registry, a `Topic()`-metadata `if` filter in every subscriber, a producer that hand-built `message.Message` + metadata, and a package doc that still advertised P0 gaps (Recoverer-outermost, no DLQ) that had in fact been closed. A primary-source review of Watermill (v1.5.2 source read directly, plus threedots.tech canon) confirmed the library's CQRS component is the intended way to remove the hand-rolled routing — and that adopting it does **not** require giving up our frozen wire contract, our transactional outbox, or our per-handler resilience stack.

Key library facts that shaped the design (verified against `components/cqrs/*.go`):

- `cqrs.CommandEventMarshaler` exposes `Name` / `NameFromMessage` / `Marshal` / `Unmarshal` precisely so naming can be customized — the stock `JSONMarshaler` uses the Go struct name, which would rename every event and break ADR 0059. A custom marshaler whose `Name(v)=v.Topic()` keeps the wire bytes stable.
- `EventProcessor.AddHandler` returns a `*message.Handler` that takes `.AddMiddleware`, so the per-handler resilience stack applies to cqrs handlers exactly as to plain ones — no need to globalize middleware or run a separate DLQ router.
- `cqrs.EventBus` is a throwaway struct (no pool, no goroutine); `watermill-sql` documents "create one publisher per transaction." So a **per-transaction** EventBus over the tx-bound forwarder publisher is the documented, intended shape for a transactional-outbox producer — not a workaround. The official `transactional-events-forwarder` example publishes via the raw tx-bound sql publisher (no EventBus) only because it does not use the cqrs component at all; it is not evidence that cqrs cannot be transactional.

## Decision

1. **One `WireAliasMarshaler` for both sides.** `Name(event)=event.Topic()` (the frozen alias, ADR 0059), `NameFromMessage=metadata[event_type]`, `Marshal`=raw `json.Marshal` + the alias on the `event_type` header (no stock struct-name "name" key), `Unmarshal`=raw `json.Unmarshal`. The producer's `EventBus` and the consumer's `EventProcessor` are configured with this one marshaler, so encode and decode share a single definition of the wire format.

2. **Produce via a per-transaction `cqrs.EventBus`.** `PublishOutbox` opens a per-tx `EventBus` over `forwarder.NewPublisher(sql.NewPublisher(TxFromPgx(tx)))`, publishing inside the aggregate's pgx.Tx (outbox-first, ADR 0064). `EventBusConfig.OnPublish` stamps `tenant_id` / `occurred_at` / the RFC 8693 act-claim (ADR 0056) / W3C trace context off the per-request ctx (EventBus sets `msg.Context(ctx)` before OnPublish). This replaces the hand-rolled `json.Marshal` + metadata loop and removes the encode/decode drift the prior producer/consumer asymmetry carried.

3. **Consume via one `cqrs.EventProcessor`.** `NewEventProcessor` hosts every module's typed `cqrs.NewEventHandler[T]` on the shared router; `GenerateSubscribeTopic` derives the module topic from the alias (`identity.*` → `identity.events`); `AckOnUnknownEvent: true` (many event types per module topic). Each handler is registered through `Router.AddCqrsHandler`, which attaches the identical per-handler resilience stack. The hand-rolled registry + per-subscriber `event_type` filter + `json.Unmarshal` boilerplate are deleted.

4. **Resilience order is load-bearing and gated.** Global (outermost first): CorrelationID → TraceContext → TenantContext. Per-handler (outermost first): PoisonQueue → Idempotency → Audit → Retry → Recoverer. `NonRetryable` errors skip Retry and dead-letter at once. Poisoned messages persist durably to `common.dead_letter`; the DLQ persister carries only Recoverer (no re-poison loop).

5. **Commands stay synchronous/direct.** Adopt EventBus/EventProcessor only, NOT CommandBus — commands are request-scoped in a monolith (ADR 0009's surviving principle); an async command bus would add latency and a broker hop for no gain.

## Consequences

- The package doc (`doc.go`) is rewritten to describe the shipped stack; the stale "known gaps" caveat (Recoverer-outermost, no DLQ) is removed — those were closed in the resilience pass.
- `cmd/worker` wires one `EventProcessor` and registers all module handlers through `AddCqrsHandler`; the dispatch (`order_packed`) handler set exists but is not yet wired (no orders producer yet).
- **Known limit — consumer subscriber sharing (tracked).** `NewEventProcessor`'s `SubscriberConstructor` currently returns one shared subscriber for every handler. This is correct for the v0.2 gochannel destination broker (per-`Subscribe` fan-out), but Watermill's own docs say a shared subscriber across handlers is what `GroupEventProcessor` is for. When the destination broker becomes a real consumer-group broker (watermill-sql / Redis / Kafka at v0.3), the constructor MUST build a per-handler subscriber with consumer group `<module>.<handlerName>` (or adopt `GroupEventProcessor`), else handlers compete for one group and fan-out silently breaks. Gated for that transition; until then it is a documented v0.3 prerequisite, not folklore.
- **Known limit — consumer ordering (tracked).** Relay-read order is library-enforced (watermill-sql queue offsets, ADR 0064) for a single forwarder. End-to-end per-aggregate ordering at the consumer is NOT guaranteed: multiple worker replicas interleave via SKIP LOCKED, per-handler Retry/backoff reorders relative to siblings, and the cqrs fan-out is parallel. Strict per-aggregate ordering, if required, needs `EventGroupProcessor` + a single-writer-per-group — a deliberate future decision, not a default.
- **Next increment — transactional inbox (tracked).** The inbox is run-then-INSERT today (at-least-once + idempotent handlers; no dedicated arch gate yet). The planned upgrade makes DB-mutating handlers effectively-once: dedup-insert + handler writes in one `pg.WithinTx` (scope from tenant metadata → `TxScopeTenant`, else `TxScopePlatform`), landing a new `TestArch_InboxIsTransactional` gate. Precondition: every must-succeed handler's repository must join the ambient ctx-tx via `pg.TxFromContext` (confirmed for CRM `Add`; the identity refresh-token-family repository must be verified before it is wrapped). External-effect handlers (email / cache / SIEM) stay at-least-once — their sends cannot be rolled back and must not hold a DB tx open across a network call.

## Fitness function

`TestArch_MessagingMiddlewareOrderResilient` + `TestArch_PoisonQueueWired` + `TestArch_MessageMetadataUsesHeaderConstants` + `TestArch_EventTypeFilterUsesConstant` + `TestArch_TopicProducerConsumerBijection` + `TestArch_OutboxForwarderUsesTxScopePlatform` + `TestArch_AppPublishesViaOutboxNotBus` + `TestArch_WatermillErrorReturnIsRetry` (all in `internal/architecture/`).

These gate, respectively: the resilience middleware order + Recoverer-innermost; the PoisonQueue/DLQ wiring; producer/consumer use of the shared `Header*` metadata constants; the consumer filter using an imported topic constant; the producer↔consumer topic bijection (the wire-alias agreement the one-marshaler decision rests on); the forwarder draining under platform scope; production code publishing via the outbox rather than a direct bus; and a Watermill handler error meaning retry. The transactional-inbox increment (see Consequences) lands its own `TestArch_InboxIsTransactional` gate.
