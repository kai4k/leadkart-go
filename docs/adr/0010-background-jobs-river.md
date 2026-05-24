# ADR 0010 — Background jobs: river (Postgres-backed)

**Status:** Accepted
**Date:** 2026-05-05

## Context

LeadKart needs background job execution for: audit log purge, idempotency table compaction, daily reports, deferred notifications, retry workflows, scheduled cleanup. The .NET version uses Hangfire on Postgres. Go has multiple job queue libraries with different backends.

LeadKart is already on Postgres (ADR 0004). Adding Redis solely for job queueing would be operational overhead.

## Decision

**`riverqueue/river`** — Postgres-backed Go job queue by Brandur Leach.

Why:
- **Postgres-native** — uses `LISTEN/NOTIFY` + advisory locks. No Redis needed.
- **Same DB as application data** — outbox + jobs in one Postgres = one operational concern.
- **Brandur Leach is the author + Crunchy Bridge runs it in production** — closest single anchor for "this works at SaaS scale".
- **OpenTelemetry support built-in** — observability story is solid.
- **Type-safe job arguments** via Go generics — `river.Worker[ArgsT]`.

Use cases planned for v0.1:
- Audit log purge (daily cron) — outbox + audit retention per ADR 0027.
- Idempotency table compaction (hourly).
- Notifications fanout (deferred from synchronous request paths).

## Consequences

**Positive:**
- Single operational concern (Postgres) for state + outbox + jobs.
- Type-safe args reduce serialization bugs.
- Brandur's production reference + active 2024–2026 development.
- River supports cron-style scheduling, retries with backoff, dead-letter queues, OTel tracing.

**Negative:**
- Postgres for jobs at very high QPS (>10K jobs/sec) needs partitioning + tuning. Not a v0.1 concern.
- `LISTEN/NOTIFY` has subtle limitations under connection pooling (works but bypasses pgBouncer transaction-pooling mode). Mitigation: dedicated pgxpool with session pooling for river specifically.

## Alternatives considered

1. **`hibiken/asynq`** — Redis-backed; mature; battle-tested. Rejected: requires adding Redis as job-queue-only dependency. River's Postgres-native approach is simpler.
2. **`hatchet`** — newer (2024), distributed workflow orchestration. Rejected for v0.1: more capability than needed; Hatchet's scope (workflow orchestration with DAGs) overlaps with sagas which we're avoiding (ADR 0031).
3. **Watermill cron** — Watermill has a cron component for recurring jobs. Rejected as primary: less feature-complete for retry/backoff/dead-letter than river. Watermill stays for messaging (ADR 0008).
4. **Custom goroutine pool + `time.Ticker`.** Rejected: no persistence across restarts; no retry backoff; reinvents what river ships.

## Sources

- [river queue project](https://github.com/riverqueue/river).
- [Brandur Leach — Introducing river](https://brandur.org/river) (2023).
- [River docs](https://riverqueue.com/docs).
- [Brandur Leach — Crunchy Bridge architecture (Postgres-only stack)](https://www.crunchydata.com/blog/author/brandur-leach).


**Fitness function:** convention-only — not mechanically expressible. River wiring is a composition-root decision; affirmative use is observed in `cmd/worker`.
