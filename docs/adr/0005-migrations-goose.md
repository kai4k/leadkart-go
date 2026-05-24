# ADR 0005 — Migrations: goose

**Status:** Accepted
**Date:** 2026-05-05

## Context

Go has three migration tools with comparable adoption in 2024–2026: `goose` (pressly), `golang-migrate/migrate`, `atlas` (ariga). All three speak SQL files; differences are in embeddability, declarative-vs-imperative, and integration footprint.

LeadKart needs:
- Embeddable as a Go library (`cmd/migrate/main.go` runs migrations programmatically).
- Up + down migrations.
- Tracks applied migrations in a metadata table (`goose_db_version`).
- Plays well with sqlc (both consume `.sql` files; same mental model).
- Pairs cleanly with Postgres RLS policies authored in plain SQL.

## Decision

**`pressly/goose/v3`** for schema migrations.

- One `.sql` file per migration in `migrations/`, timestamp-prefixed (`20260101_000001_initial.sql`).
- `-- +goose Up` / `-- +goose Down` markers per file.
- `cmd/migrate/main.go` runs migrations programmatically via the goose Go API.
- Available via Taskfile: `task migrate:up`, `task migrate:new`.

Migrations include RLS policy authoring directly:

```sql
-- +goose Up
CREATE TABLE identity.tenants (...);
ALTER TABLE identity.tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.tenants
    USING (id = current_setting('app.tenant_id')::uuid);
```

## Consequences

**Positive:**
- Embeddable: integration tests run `goose.Up(db, "migrations")` programmatically against testcontainers Postgres.
- Plain SQL files — no DSL to learn; reviewable as PR diffs.
- Pairs naturally with sqlc (both consume `.sql`).
- RLS policies authored directly in migration SQL — no special tooling.

**Negative:**
- No declarative diffing. If schema drifts from migration history, reconciliation is manual.
- Down migrations must be hand-maintained (goose runs them via `goose down` but won't auto-generate).

## Alternatives considered

1. **`golang-migrate/migrate`** — most stars (~16k), CLI-first design. Embeddable Go API exists but less ergonomic. Rejected for ergonomic reasons; goose's Go API is cleaner for `cmd/migrate/main.go`.
2. **`ariga/atlas`** — declarative schema-as-code with diff engine. Powerful but advanced features (RLS policy management) gated behind Atlas Pro paywall. Rejected: cost not justified for v0.1; can revisit if declarative schema becomes load-bearing.
3. **`amacneil/dbmate`** — simpler than goose, no Go embeddable API. Rejected.
4. **`rubenv/sql-migrate`** — older, declining adoption.

## Sources

- [pressly/goose v3 docs](https://github.com/pressly/goose).
- [Ent versioned migrations doc](https://entgo.io/docs/versioned-migrations/) — confirms goose as supported migration tool option.
- [Brandur Leach — All In on sqlc/pgx](https://brandur.org/sqlc) — Crunchy Bridge production reference (uses similar SQL-file migration pattern).


**Fitness function:** convention-only — not mechanically expressible. Migration tool choice is a build-time decision; the migration runner (`cmd/migrate`) is the load-bearing piece + already covered by `task ci:migrations`.
