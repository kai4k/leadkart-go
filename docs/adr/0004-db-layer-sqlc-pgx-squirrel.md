# ADR 0004 — DB layer: sqlc + pgx/v5 + squirrel

**Status:** Accepted
**Date:** 2026-05-05
**Supersedes:** preliminary considerations of Bun, Ent, bob, GORM during planning (see `.claude/plans/snazzy-strolling-cray.md` §G.D + Q2 for full evaluation).

## Context

The Go DB-access landscape 2024–2026 has two main camps:
- **SQL-first codegen** — sqlc generates typed Go from `.sql` files. Stripe Go-internal, Sourcegraph, Crunchy Bridge, Brandur Leach.
- **Typed builders / ORMs** — Ent, Bun, bob, GORM. EF-Core-flavoured ergonomics with various levels of magic.

Three architectural requirements expose ORM weaknesses for LeadKart specifically:
1. **Postgres RLS + `SET LOCAL app.tenant_id`** per-transaction tenant context.
2. **Watermill outbox** in same `*sql.Tx` as aggregate writes.
3. **Recursive CTEs** for hierarchy (subordinates) + permission filters.

Ent fails all three (Atlas Pro for declarative RLS; open issue [ent#4295](https://github.com/ent/ent/issues/4295) for `pgx.Tx` access; recursive CTEs require `sql/execquery` escape hatch which bypasses interceptors). Bun clears the hurdles but introduces 5–10% real-world latency overhead and ~4k stars vs sqlc's ~14k. bob is single-maintainer, explicitly rejects soft-delete + audit timestamps. GORM is universally discouraged for new production code by senior Go community.

## Decision

**`sqlc + pgx/v5 + squirrel + goose`** — locked.

- **`sqlc`** — codegens typed Go functions from `.sql` files. Compile-time type safety, zero runtime reflection, near-raw-pgx performance.
- **`pgx/v5`** — canonical Postgres driver. Standalone (not via `database/sql` bridge unless required). Stripe / Cloudflare / Crunchy adoption.
- **`squirrel`** (Masterminds/squirrel) — runtime SQL builder for the 2–3 dynamic-query spots (CRM lead filter, Platform marketplace search, dashboard reports). Small surface area; doesn't replace sqlc.
- **`goose`** — schema migrations (ADR 0005).

Repository pattern: TDL **`UpdateFn` shape** ([TDL Sep 2024](https://threedots.tech/post/database-transactions-in-go/)):

```go
type Repository interface {
    Add(ctx context.Context, t *Tenant) error
    UpdateByID(ctx context.Context, id TenantID, updateFn func(*Tenant) (bool, error)) error
    GetByID(ctx context.Context, id TenantID) (*Tenant, error)
}
```

The closure mutates the aggregate; the repo wraps load + closure call + persist + outbox-write in one transaction. Outbox-write is structurally inseparable from the aggregate update.

## Consequences

**Positive:**
- SQL files in PRs — every WHERE clause grep-able for RLS audit.
- Compile-time type safety on every query.
- `*pgx.Tx` is the natural API — Watermill, river, LISTEN/NOTIFY all consume cleanly without bridge layers.
- Recursive CTEs first-class — write `WITH RECURSIVE` directly in `.sql`.
- Brandur's 700-query Crunchy Bridge production reference is the closest single anchor for "this works at SaaS scale".

**Negative:**
- ~150 hand-written CRUD queries across 8 modules (one afternoon per module). Real cost but bounded.
- N+1 risk if developer writes 5 separate queries when one JOIN suffices. Mitigation: PR review + benchmark hot endpoints.
- Eager loading verbose — relation fetches use hand-written `LEFT JOIN ... json_agg(...)` patterns.

## Switch trigger

If, mid-execution:
- Dynamic-query proliferation in CRM/Platform/Reports exceeds ~30 distinct shapes, AND
- Retrospectives flag `.sql` ceremony as a sustained drag,

…evaluate **Bun** for those specific modules (NOT a project-wide flip). Per-module adapter swap via the `domain.Repository` interface is one PR.

## Alternatives considered

| Option | Why rejected |
|---|---|
| **Ent** | Fails 3 LeadKart hurdles (RLS bridge, Watermill `*sql.Tx`, recursive CTEs force escape hatches); Atlas Pro paywall; 60%+ benchmark overhead vs raw pgx; multiple negative production reports ([shandoncodes](https://dev.to/shandoncodes/stop-using-entgoplease-5gm5), [luketic.de](https://luketic.de/2024/04/19/beware-of-ent-entgo-io/)). |
| **Bun** | Clears hurdles + EF-Core ergonomics; smaller community (~4k stars); 5–10% real-world latency overhead. Acceptable per-module fallback if sqlc ceremony becomes painful. |
| **bob** | Single-maintainer (1.7k stars); explicitly rejects soft-delete + audit timestamps. |
| **GORM** | Senior community discourages (Brandur, Cheney, Kennedy) — runtime magic, hidden N+1, schema drift. |

## Sources

- [Brandur Leach — How We Went All In on sqlc/pgx](https://brandur.org/sqlc).
- [Brandur Leach — sqlc 2024 check-in](https://brandur.org/fragments/sqlc-2024).
- [TDL — Database Transactions in Go (UpdateFn pattern, Sep 2024)](https://threedots.tech/post/database-transactions-in-go/).
- [TDL — Distributed Transactions in Go (outbox pattern, Oct 2024)](https://threedots.tech/post/distributed-transactions-in-go/).
- [Glukhov — Comparing Go ORMs Sept 2025](https://www.glukhov.org/post/2025/09/comparing-go-orms-gorm-ent-bun-sqlc/).
- [Bytebase — Choose the Right Go ORM 2025](https://www.bytebase.com/blog/golang-orm-query-builder/).
- [efectn/go-orm-benchmarks](https://github.com/efectn/go-orm-benchmarks/blob/master/results.md) — benchmark numbers.
