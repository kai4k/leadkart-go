# ADR 0003 — Persistence default: state-based + outbox

**Status:** Accepted
**Date:** 2026-05-05
**Supersedes context from:** LeadKart .NET (which used Marten event sourcing for Orders).

## Context

LeadKart .NET used Marten event sourcing for Orders + audit log. The Go ecosystem has **no Marten-equivalent** as of 2026:
- `hallgren/eventsourcing` is closest but lacks upcasting; pre-1.0 maintenance.
- `KurrentDB-Client-Go` (formerly EventStoreDB) requires an external store.
- `ThreeDotsLabs/esja` is pre-1.0 with explicit "unstable API" notice.

The cost of event sourcing is materially higher in Go than in .NET. Three Dots Labs' newer EDA training does not even cover ES as a core topic — they focus on outbox + sagas + process managers. Brandur Leach's [Crunchy Bridge architecture](https://brandur.org/fragments/events) explicitly rejects ES for typical SaaS in favour of a simple `events` table with mandatory retention.

## Decision

**Default persistence: state-based + outbox-in-Postgres.**

For every aggregate, the repository:
1. Persists current state in a typed Postgres table (sqlc + pgx, ADR 0004).
2. Appends domain events to an `outbox` table in the **same transaction** as the state write.
3. A separate Watermill SQL forwarder polls the outbox and republishes to the in-process Go-channel pubsub (modular monolith) or external broker (later, if split).

The outbox table **doubles as the audit log** (Brandur "events table" pattern, ADR 0027).

Event sourcing reserved as a **per-aggregate decision** when load-bearing: temporal queries, replay-driven compensation, regulatory record-history needs. Concrete rule: **zero modules use ES at v0.1** (ADR 0035).

## Consequences

**Positive:**
- Cheaper than ES — no projection layer, no upcaster machinery, no snapshot tuning.
- Single source of truth: row state in Postgres. Audit/replay covered by outbox table.
- Watermill outbox + forwarder is canonical TDL pattern with documented examples.
- DPDP/GDPR right-to-erasure straightforward (UPDATE + DELETE on state rows).

**Negative:**
- No "as-of-date" temporal queries without explicit history columns.
- No free replay — if business asks "rebuild Orders state from scratch", we don't have the history.
- Outbox retention TTL must be explicit (Brandur recommends 90-day hard cap; LeadKart picks 7-year for audit per DPDP/SOC2 compliance with cold-storage export).

## Alternatives considered

1. **Event sourcing for Orders + Inventory** (matching .NET decision). Rejected for Go: no Marten-equivalent; cost too high without library support; outbox covers audit/replay needs sufficiently for v0.1. Revisit per ADR 0035 if specific aggregate proves load-bearing.
2. **Plain pgx event tables (hand-rolled ES)**. Rejected as default: complexity per aggregate exceeds benefit when most modules don't need temporal queries.
3. **ESDB / Kurrent as external store**. Rejected: adds operational dependency; no clear payback for v0.1 scale.

## Sources

- Brandur Leach — [There's always an events table](https://brandur.org/fragments/events) (Crunchy Bridge production reference).
- Three Dots Labs — [Distributed Transactions in Go: Read Before You Try](https://threedots.tech/post/distributed-transactions-in-go/) (Oct 2024) — outbox pattern recipe.
- Three Dots Labs — [Database Transactions in Go with Layered Architecture](https://threedots.tech/post/database-transactions-in-go/) (Sep 2024) — UpdateFn pattern for outbox-in-tx.
- Greg Young — *CQRS Documents* (2010) — original ES decision criteria; cost vs benefit.
- Chris Richardson — *Microservices Patterns* ch.3 (Outbox).


**Fitness function:** convention-only — not mechanically expressible. State-vs-event-sourcing is a doctrine choice; mechanically asserting "state-based" requires walking every aggregate's persistence path. Re-evaluate if event sourcing ever lands.
