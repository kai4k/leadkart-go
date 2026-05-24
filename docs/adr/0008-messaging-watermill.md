# ADR 0008 — Messaging: Watermill v1.5+ + watermill-sql outbox

**Status:** Accepted
**Date:** 2026-05-05

## Context

LeadKart's modular monolith (ADR 0001) uses cross-context events for module-to-module communication: Identity emits `TenantRegistered` → Platform initialises lead credits + CRM seeds default pipeline. Same-binary in v0.1, splittable to microservices later.

The Go ecosystem has one canonical messaging library: **Watermill** (Three Dots Labs). v1.5 (Sep 2025) is current. Stable v1 line since 2019; no v2.x exists.

## Decision

**Watermill v1.5+ with watermill-sql outbox + forwarder pattern.**

Architecture per [TDL "Distributed Transactions in Go" (Oct 2024)](https://threedots.tech/post/distributed-transactions-in-go/) + Watermill `_examples/real-world-examples/transactional-events-forwarder`:

1. **Aggregate emits domain events** into an internal slice on mutation.
2. **Repository's `UpdateByID` closure pattern** (ADR 0004) writes aggregate state + appends domain events to module's `outbox` table — same `*pgx.Tx`.
3. **Forwarder process** subscribes to the SQL outbox topic via `watermill-sql v4`'s `sql.BeginnerFromStdSQL(db)` constructor, republishes to **in-process Go-channel pubsub** (modular monolith).
4. **Subscribers in other modules** (`internal/{other}/ports/events.go`) consume integration events + translate to `app.Commands.X.Handle(...)` calls.
5. **Idempotency** — subscriber tracks `(message_id, handler_name)` in `processed_messages` table; insert with `ON CONFLICT DO NOTHING`.

**Watermill API specifics for LeadKart (v1.4 / v1.5):**
- Use `sql.BeginnerFromStdSQL(db)` — `watermill-sql v4` constructor (Mar 2026 release).
- Use `router.AddConsumerHandler(...)` — NOT deprecated `AddNoPublisherHandler`.
- Use `cqrs.ProtoMarshaler` — NOT deprecated `cqrs.ProtobufMarshaler`.
- Use `cqrs.CommandEventMarshalerDecorator` to cut marshaling boilerplate.
- New PostgreSQL Requeuer (v1.4) for poison-message handling.

**No external broker at v0.1.** In-process Go-channel pubsub is sufficient for modular monolith. When the project splits modules into separate binaries (microservices), swap pubsub for Redis/NATS/Kafka — Watermill abstracts the change.

## Consequences

**Positive:**
- Outbox-in-Postgres guarantees at-least-once delivery without distributed transactions.
- Outbox row doubles as audit log (ADR 0027).
- Forwarder process cleanly separates write path from publish path.
- Watermill's pluggable backend means microservices migration is a config change, not a rewrite.

**Negative:**
- Eventual consistency between modules — cross-context reads may briefly see stale state.
- Forwarder is a separate process (`cmd/worker`) — operational complexity.
- Outbox table grows unboundedly without retention. Mitigation: 7-year retention via daily river job + cold-storage export (ADR 0027).

## Watermill rules

- All integration events have `event_type` metadata header (`identity.tenant_registered.v1`).
- Versioning via metadata — v1, v2 events carry distinct types; consumers handle either or both during transition.
- Subscribers are **idempotent** by contract — duplicate delivery is tolerated by design.
- Sagas explicitly **discouraged** (ADR 0031) — merge bounded contexts before introducing.

## Alternatives considered

1. **NATS / Kafka direct (no Watermill).** Rejected: ties domain code to broker SDK; no abstraction for monolith → microservices migration.
2. **Application-level events without outbox.** Rejected: no at-least-once guarantee under crash; cross-tx race conditions.
3. **Database CDC (Debezium → broker).** Rejected for v0.1: operational complexity; outbox + Watermill forwarder is simpler and sufficient at LeadKart's scale.

## Sources

- [Watermill v1.5 release (Sep 2025)](https://threedots.tech/post/watermill-1-5/) + [v1.4 release (Oct 2024)](https://threedots.tech/post/watermill-1-4/).
- [Watermill `_examples/real-world-examples/transactional-events-forwarder`](https://github.com/ThreeDotsLabs/watermill/tree/master/_examples/real-world-examples).
- [TDL — Distributed Transactions in Go: Read Before You Try (Oct 2024)](https://threedots.tech/post/distributed-transactions-in-go/).
- [TDL — Database Transactions in Go with Layered Architecture (Sep 2024)](https://threedots.tech/post/database-transactions-in-go/) — UpdateFn pattern.
- Chris Richardson — *Microservices Patterns* ch.3 (Outbox).


## Fitness function

`TestArch_SubscribersInPortsSubscribers + TestArch_OutboxTableSchema` (in `internal/architecture/`).

Subscribers live only in `ports/subscribers/`; every outbox table declares the canonical column set (id, occurred_at, topic, payload, forwarded_at).
