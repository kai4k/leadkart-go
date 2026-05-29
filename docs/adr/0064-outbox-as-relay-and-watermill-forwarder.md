# ADR 0064 — Outbox is a pure relay; adopt Watermill Forwarder + watermill-sql; RLS where it matters

**Status:** Accepted
**Date:** 2026-05-29
**Supersedes (in part):** [ADR 0027](0027-audit-log-outbox.md) — the "outbox doubles as the audit log" coupling.
**Amends:** [ADR 0006](0006-multi-tenancy-rls.md) (RLS scope), [ADR 0004](0004-db-layer-sqlc-pgx-squirrel.md) (data-access).

## Context

A deep audit (2026-05-29) found that our hand-rolled per-module outbox forwarder was justified, in earlier notes, as "Watermill/`watermill-sql` can't fit our RLS + schema-per-module outbox." Primary-source research disproved the load-bearing part of that claim:

- **TDL is the highest authority here, and TDL says: use the `Forwarder`.** Their "Distributed Transactions in Go" article wires Watermill's `Forwarder` + `watermill-sql`; hand-rolling "is not trivial." TDL says **nothing** about Postgres RLS — there was never a TDL position making RLS incompatible with the library.
- **`watermill-sql` is customizable, not incompatible.** `SchemaAdapter` lets you target an arbitrary table; the `Forwarder` mandates no table shape; the consume SELECT runs in a transaction you control (a session GUC works). "Incompatible" was false.
- **The real driver was our own DB design** — [ADR 0027](0027-audit-log-outbox.md)'s coupling of *event relay* and *permanent per-tenant audit log* into one table forced never-delete + RLS, which clashed with the library's offset/ack consume model. And that coupling was **already half-abandoned**: live audit reads go to `buildingblocks.audit_log_entry` (no RLS); the per-tenant outbox audit query was dead code. So RLS on the outbox protected an access path that does not exist.
- **The library is also more correct on ordering.** Our `ORDER BY created_at, id` + `FOR UPDATE SKIP LOCKED` drain still has the classic serial-gap bug (a late-committing tx with an earlier timestamp can be skipped). `watermill-sql` v4 closes it with an `xid8` transaction-id + `pg_snapshot_xmin` visibility predicate.

## Decision

1. **The outbox is a pure event relay.** It is written once (same tx as the aggregate, outbox-first canon — survives from ADR 0027), drained, and forwarded. It is **not** an audit log. Audit stays in `buildingblocks.audit_log_entry`. The dead `ListAuditEventsForTenant` outbox query is retired.

2. **Adopt Watermill's `Forwarder` + `watermill-sql` v4** (TDL canon) for the relay→broker hop, replacing the four hand-rolled forwarders. Domain fields (`tenant_id`, `occurred_at`, `act_operator_id`/`session`/`reason`) travel in `message.Metadata` — which is where the consumer side (`TenantContextMiddleware`, `AuditMiddleware`, RFC 8693 act-claim per ADR 0056) already reads them. The library's `xid8`/snapshot ordering replaces the hand-rolled `created_at,id` + `SKIP LOCKED` drain and removes the serial-gap bug.

3. **RLS where it matters** (see [ADR 0006](0006-multi-tenancy-rls.md) Amendment 1). RLS belongs on the **tenant data plane** — tables holding tenant-owned rows reachable through tenant-scoped query paths. The outbox/relay is platform-only infrastructure (read solely by the forwarder under platform scope), so it carries **no RLS**. The deciding test is the access path, not the presence of a `tenant_id` column.

4. **The blanket-RLS gate becomes access-path-aware.** `TestArch_EveryTenantTableHasRLSAndForce` no longer forces RLS on relay/infra tables; they opt out via the existing `-- arch-test:opt-out-rls` marker (or are library-managed and out of the gate's scope).

## Consequences

- Migration: outbox tables lose RLS + the bespoke `forwarded` flag in favour of the library's schema/offset model; the four `OutboxForwarder` structs are deleted. This is a messaging-spine change validated in cloud CI (no local Docker).
- `cmd/worker` wires one `forwarder.Forwarder` per module's relay subscriber; producers publish via the forwarder-decorated `sql.Publisher` inside the aggregate tx.
- Pitfalls to respect (per `watermill-sql` v4): `AckDeadline=0` stalls `xmin` visibility; the `forwarder.Marshaler` must match on both ends; non-enveloped rows are nacked unless handled.
- Outbox-first + same-tx-write (ADR 0027's surviving principle) is unchanged.

## Fitness function

`TestArch_OutboxForwarderUsesTxScopePlatform` + `TestArch_NoDirectOutboxStateAssertions` + `TestArch_EveryTenantTableHasRLSAndForce` (in `internal/architecture/`).

The forwarder reads under platform scope; tests observe outbox emissions via the bus (never by reading the table); and the RLS gate — once made access-path-aware — asserts tenant-data tables keep RLS while the relay opts out. The Watermill-adoption-specific gate (assert the relay is wired through `forwarder.Forwarder`, not a bespoke poller) lands with the Phase-5 implementation.
