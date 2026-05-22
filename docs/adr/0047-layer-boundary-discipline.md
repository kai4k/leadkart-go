# ADR 0047 — Layer-boundary discipline (`app/` → no DB driver, no sqlc, no concrete adapter)

**Status:** Accepted
**Date:** 2026-05-22

## Context

The Identity module follows the TDL hexagonal layout (per ADR 0002 + ADR 0009): `domain/` → `app/` → `ports/` and `adapters/`. Dependencies point INWARD — `domain/` knows nothing about `app/`, `app/` knows nothing about `ports/` or `adapters/`. **In practice, parts of `app/` had drifted:**

| File | Violation |
|---|---|
| `app/query/audit.go` | imported `internal/identity/adapters/db` (sqlc-generated row types) + `pgxpool` + `pgtype` |
| `app/query/search.go` | same set of imports + used `db.SearchPersonsByTextParams` directly |
| `app/query/platform_stats.go` | imported `pgxpool` + `pgx` (raw SQL inline in the handler) |
| `app/command/register_tenant.go` | depended on `*adapters.TenantRepository`/`PersonRepository`/`MembershipRepository`/`RoleRepository` (CONCRETE structs) + `pgx.Tx` in helper signatures |
| `app/command/user_create.go` | same as register_tenant |

The first three are HARD breaks: app code reaches into the database driver + sqlc-generated package. The last two are an inversion of Cheney's "accept interfaces, return structs" — multi-aggregate write handlers depended on concrete repository structs because they needed `AddInTx(ctx, tx, agg)` for a same-tx multi-aggregate write, and pgx.Tx leaked through.

Industry canon for clean-architecture boundary enforcement:

| Source | Position |
|---|---|
| **TDL Wild Workouts** (Nov 2025) | Repository interfaces live in `domain/`; concrete impl in `adapters/`. `app/` depends ONLY on interfaces. No pgx leakage. |
| **Vladimir Khorikov** ("Pragmatic Clean Architecture") | Application layer = pure orchestration over domain abstractions. Database driver is infrastructure; application MUST NOT know it exists. |
| **Brandur Leach** (Crunchy Bridge) | Repository pattern with ctx-based unit-of-work; tx is propagated via `context.Context`, never as a typed parameter. |
| **Russ Cox / Cheney** ("Accept interfaces, return structs") | Consumer-defined interfaces; concrete types are returned, never demanded by API contracts. |
| **Robert C. Martin** (Clean Architecture book §22) | Boundary crossing = abstract types only. No infrastructure types cross inward. |

Without a CI gate, the drift WILL recur — Phase 2 work on Platform / CRM / Orders modules will copy the violating shape if it goes unchallenged.

## Decision

**`app/` is a pure-Go region. It MAY depend on:**

- `internal/.../domain/` — domain entities, value objects, repository INTERFACES, sentinel errors
- `internal/common/*` — pure substrates (clock, ids, slug, email, errs, pagination, tenancy)
- `internal/platform/cache` — the HybridCache facade interface
- `internal/platform/pg` — the `pg.UnitOfWork` INTERFACE + `pg.TxScope` enum + `pg.TxFromContext` (read-only — adapter-internal helpers)
- `internal/platform/audit` — the `audit.Reader` interface + `audit.Entry` value type
- `internal/identity/integrationevents` — integration-event V1 records (wire-shape constants)
- stdlib + `github.com/google/uuid` + `golang.org/x/sync/*`

**`app/` MUST NOT depend on:**

- `internal/identity/adapters/db` — sqlc-generated row types (`db.IdentityTenant`, `db.BuildingblocksAuditLogEntry`, `db.Queries`, etc.) leak the persistence schema into the application layer. They are an internal detail of the adapter, not a contract.
- `internal/identity/adapters` (the parent package) — concrete repository structs (`*adapters.TenantRepository`) are an inversion of Cheney; handlers must accept the domain `tenant.Repository` interface.
- `github.com/jackc/pgx/v5` (driver) / `pgx/v5/pgxpool` (pool) / `pgx/v5/pgtype` (type wrappers) — the SQL driver is substrate. App code that needs a transaction uses `pg.UnitOfWork`, not pgx directly.
- `github.com/Masterminds/squirrel` — query builder, adapter-only.

### Patterns enabling the discipline

#### 1. Consumer-defined reader interfaces for read-side queries

When a query handler needs cross-aggregate or audit-log reads, define the interface NEXT TO the consuming handler (Cheney), with a concrete pg-backed implementation in `adapters/`:

```go
// internal/platform/audit/reader.go — interface
type Reader interface {
    ListByTenant(ctx context.Context, tenantID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]Entry, error)
    ListByUser(ctx context.Context, userID uuid.UUID, before time.Time, beforeID uuid.UUID, limit int32) ([]Entry, error)
}

// internal/identity/adapters/audit_reader_pg.go — concrete impl
type AuditReaderPG struct { pool *pgxpool.Pool; tx *pg.Transactor; q *db.Queries }
func (r *AuditReaderPG) ListByTenant(...) ([]audit.Entry, error) { /* uses sqlc */ }

// internal/identity/app/query/audit.go — handler
type ListAuditEventsByTenantHandler struct { reader audit.Reader }
```

Same pattern applied to `query.SearchIndex` + `query.PlatformStatsReader` in this PR.

#### 2. UnitOfWork interface for multi-aggregate same-tx writes

`pg.UnitOfWork` lives in `internal/platform/pg/uow.go`:

```go
type UnitOfWork interface {
    WithinTx(ctx context.Context, scope TxScope, fn func(ctx context.Context) error) error
}
```

The active `pgx.Tx` is stashed in ctx via `pg.contextWithTx` (unexported) and retrieved by ADAPTER code via `pg.TxFromContext(ctx)`. The handler closure never sees the tx — it just calls `h.tenants.Add(ctx, t)`, and the repository's `Add` method checks `pg.TxFromContext(ctx)` to decide whether to join the surrounding tx or open its own.

This is the canonical Brandur / sqlc-pattern unit-of-work. `*pg.Transactor` satisfies `UnitOfWork` via a thin wrapper around its low-level `WithinTxPgx` (the adapter-facing variant that exposes `pgx.Tx` directly — used inside `adapters/` for the per-aggregate `addOnTx` helpers).

```go
// internal/identity/app/command/register_tenant.go
type RegisterTenantHandler struct {
    uow         pg.UnitOfWork          // interface
    tenants     tenant.Repository       // interface
    persons     person.Repository       // interface
    memberships membership.Repository   // interface
    roles       role.Repository         // interface
}

func (h) persistAggregatesInTx(ctx, cmd, existing, pwd) (Result, error) {
    err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
        // ctx carries the active tx — repository .Add joins it
        if err := h.tenants.Add(ctx, t); err != nil { ... }
        if err := h.persons.Add(ctx, p); err != nil { ... }
        if err := h.memberships.Add(ctx, m); err != nil { ... }
        return nil
    })
}
```

No `pgx.Tx` in the handler. No concrete adapter struct. Pure interface-driven.

#### 3. Adapter `Add` joins context tx

Each repository's `Add` method follows this shape:

```go
func (r *TenantRepository) Add(ctx context.Context, t *tenant.Tenant) error {
    if tx, ok := pg.TxFromContext(ctx); ok {
        return r.addOnTx(ctx, tx, t)   // join surrounding UnitOfWork tx
    }
    return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx, tx pgx.Tx) error {
        return r.addOnTx(ctx, tx, t)   // open own tx
    })
}

func (r *TenantRepository) addOnTx(ctx, tx pgx.Tx, t *tenant.Tenant) error { /* persist + drain events */ }
```

`addOnTx` is unexported — handler code MUST go through `Add` (interface method), which automatically routes based on ctx.

### Enforcement — arch test as CI gate

`internal/identity/app/arch_test.go` walks every non-test `.go` file under `app/` and fails CI on any import in the forbidden list:

```go
var forbiddenAppImports = map[string]string{
    "github.com/leadkart/leadkart-go/internal/identity/adapters/db": "sqlc-generated row types leak DB shape into the application layer",
    "github.com/leadkart/leadkart-go/internal/identity/adapters":    "concrete adapter package; handlers must accept domain interfaces",
    "github.com/jackc/pgx/v5":                                       "pgx driver is a substrate concern",
    "github.com/jackc/pgx/v5/pgxpool":                               "pgxpool is a substrate concern",
    "github.com/jackc/pgx/v5/pgtype":                                "pgtype is a sqlc/driver concern",
}
```

Test files are exempt — fixtures may wire real adapters against testcontainers. The rule guards production code only.

**Per CLAUDE.md "drift = finding": if this test fails, fix the import. DO NOT amend the allowlist without an ADR amendment.**

## Consequences

**Positive:**

- **Boundary discipline as a CI gate**, not a convention reviewers must remember. Drift is impossible — the test fails on every PR that introduces a forbidden import.
- **Handlers are now driver-agnostic.** Future Postgres → MySQL / SQLite / DynamoDB migration touches only `adapters/`; `app/` unchanged.
- **Test fakes become trivial.** A handler that accepts `tenant.Repository` can be unit-tested with an in-memory map; no testcontainers, no sqlc, no pgxpool. Phase 2+ test surface area shrinks.
- **TDL Wild Workouts canon achieved.** The shape now matches the reference: `app/` is a pure orchestration layer over domain abstractions.
- **Better AI-assistant comprehension.** Claude / Copilot reading a command handler sees only domain types — the "why is this struct here" question doesn't arise.

**Negative:**

- **One more interface to maintain.** `pg.UnitOfWork` is a small addition but it IS additional surface. Mitigated by being a single-method interface.
- **Adapter `Add` methods have a 4-line "check ctx for tx" preamble.** Repetitive across 5 repositories. Could be DRY-ed with a generic helper if it grows past 5; acceptable boilerplate at this scale.
- **Pattern cost for new module authors.** Phase 2 Platform / CRM authors must learn the reader-interface + UnitOfWork patterns. Mitigated by ADR 0047 + the existing adapters as reference. Arch test catches mistakes immediately.

## Alternatives considered

1. **Move the rule to a code-review checklist instead of a CI gate.** Rejected. Review fatigue WILL cause drift. The arch test is ~120 lines + free at CI time.

2. **Use a third-party arch-test library (`mattermost/mattermost-server/architecture` style).** Rejected. Single-file stdlib `go/parser` walk is simpler, faster, and has zero dep cost.

3. **Allow `pgx.Tx` to leak into `app/` as a "TDL pragmatic exception" because of multi-aggregate writes.** Rejected. The user explicitly flagged this as a strict-forbidden rule; the UnitOfWork pattern eliminates the apparent necessity.

4. **Move sqlc generation per-module so each module has its own `db` package.** Considered. Adds sqlc config complexity for no real gain at v0.2 — the boundary rule is enforced by import path regardless of where `db` lives.

5. **Build a generic `Repository[T]` wrapper to remove the per-adapter `addOnTx` boilerplate.** Premature; the boilerplate is 4 lines × 3 repos = 12 lines total. Revisit if it grows past 8 repositories.

## Sources

- ADR 0001 — Modular monolith (the layer scheme this enforces)
- ADR 0002 — Hexagonal + DDD (the boundary contract)
- ADR 0004 — sqlc + pgx + squirrel (the persistence stack the rule walls off)
- ADR 0009 — Application{Commands, Queries} facade (the handler shape)
- ADR 0037 — sqlc layout (`db` subpackage as the violation target)
- TDL Wild Workouts repo — `internal/trainings/app/*.go` (handlers depend on interfaces only); `internal/trainings/adapters/*.go` (concrete pgx + sqlc impls).
- Vladimir Khorikov — *Pragmatic Clean Architecture* (the application-layer-as-pure-orchestration treatment).
- Brandur Leach — *"Implementing Stripe-like Idempotency Keys in Postgres"* + river-queue source (ctx-propagated tx pattern as canonical Go shape).
- Dave Cheney — *"Practical Go: Real-world advice for writing maintainable Go programs"* §"Accept interfaces, return structs."
- Russ Cox — Go blog "Codebase Refactoring (with help from Go)" (the principle of consumer-defined interfaces).
