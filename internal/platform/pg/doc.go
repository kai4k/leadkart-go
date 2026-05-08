// Package pg holds platform-level Postgres primitives shared across modules:
// the migrations test plus pgxpool factory + tenancy
// `SET LOCAL app.tenant_id` helpers consumed by every module's repository
// adapter.
//
// Per ADR 0006 (multi-tenancy via Postgres RLS + SET LOCAL) and ADR 0019
// (testing with testcontainers).
//
// # Tenant binding contract
//
// RLS scoping in LeadKart is per-transaction, not per-connection. Every
// state-changing flow MUST go through [Transactor.WithinTx] so the
// transactor:
//
//  1. Opens a tx (any pool checkout fine).
//  2. Calls [SetTenantOnTx] (or [SetPlatformOnTx]) — issues
//     `SELECT set_config('app.tenant_id', $1, true)` so the GUC is
//     transaction-scoped (`SET LOCAL`-equivalent).
//  3. Runs the closure; commits or rolls back.
//
// We deliberately do NOT use a pgxpool AfterAcquire callback to bind
// tenant on connection-acquire: pooled connections are reused across
// requests for different tenants; AfterAcquire would either need to
// be a no-op (defeating the purpose) or run a SET that bleeds across
// requests (catastrophic cross-tenant read). Per-tx binding via
// [Transactor.WithinTx] is the only correct shape.
//
// Direct `pool.Query` / `pool.Exec` calls against tenant-scoped tables
// are forbidden — they would run without tenant binding + return zero
// rows under RLS or (worse) leak across tenants if RLS were ever
// FORCE-disabled. Adapter code MUST go through the transactor.
package pg
