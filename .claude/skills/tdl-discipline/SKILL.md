---
name: tdl-discipline
description: TDL-strict patterns for Go domain/app/ports/adapters. Use BEFORE writing or reviewing any code under internal/*/domain, app, ports, or adapters — aggregates, repositories, command/query handlers, subscribers, outbox. Enforces the Wild Workouts / Three Dots Labs shape this repo is built on.
---

# TDL discipline (LeadKart-Go)

Authoritative doctrine: `docs/doctrine/tdl_canon.md` (the *why*). This skill is the
operational checklist. Trust order: tdl_canon → ADRs → code. Code drift = a finding.

## Layers
- `domain/` — entities, value objects, repository INTERFACES. No infra imports, no pgx, no validate-tags. Pure Go + injected `now func() time.Time`.
- `app/` — command + query handlers. The handler **IS** the orchestrator AND the contract. No `service/` layer, no mediator, no DI container, no dep-count cap. `app/` may NOT import pgx/pgxpool/sqlc-db/concrete-adapters (ADR 0047); it depends on domain interfaces + `pg.UnitOfWork`.
- `ports/` — inbound HTTP handlers + event subscribers (concrete).
- `adapters/` — outbound: sqlc/pgx repos, publishers. Interfaces live with their consumer, never in ports/ or adapters/.

## Aggregates
- Factory `NewX(...) (*X, error)` enforces invariants; rehydration `UnmarshalFromDB(...)` does NOT validate or emit events.
- No public setters. Mutators are behavior methods that emit domain events on state change.
- Strongly-typed IDs. No nil-able domain primitives where a VO fits.

## Repositories (TDL UpdateFn)
- `Add(ctx, *Agg) error` for new; `UpdateByID(ctx, id, func(*Agg)(bool,error)) error` for changes — repo owns the tx; `(true,nil)` persists+drains events, `(false,nil)` no-ops, `(_,err)` rolls back.
- NO business-verb repo methods (no `RevokeFamily`, `MarkSold`). NO `Save()`. NO DB types in domain signatures.
- Adapters JOIN an ambient UoW tx via `pg.TxFromContext(ctx)`; else open their own (`if tx,ok:=pg.TxFromContext(ctx);ok{...} else {WithinTxPgx...}`).

## Transactions
- Single-aggregate command → bare `UpdateByID` (no UoW wrapper — that's ceremony).
- Genuine multi-aggregate atomic invariant (same module) → `pg.UnitOfWork.WithinTx(ctx, scope, fn)` (the Transaction-Provider). One-aggregate-per-command is the default.
- Cross-MODULE effect → outbox event + subscriber (EDA), never a shared tx. `TestArch_NoCrossModuleImports` enforces.
- tx-in-context: the active `pgx.Tx` is the ONE permitted domain-adjacent ctx value, adapter-only via `pg.TxFromContext` — a documented, deliberate deviation from TDL's no-tx-in-ctx.

## Messaging (current canon — ADR 0067)
- Produce: per-tx `cqrs.EventBus` over watermill-sql + Forwarder, in the aggregate tx (outbox-first).
- Consume: `cqrs.EventProcessor` typed handlers; delivery = at-least-once + idempotent handlers (NOT a transactional inbox — evaluated + reverted). Every consumer handler declares its idempotency strategy (`arch-test:idempotency-via-*`).

## Tests (ADR 0062)
- Per-aggregate fakes co-located in `<aggregate>test/`. Table-driven + `t.Run`.
- Domain rules → pure unit; orchestration → unit vs FakeRepository; SQL contract → integration only.
- Integration tests are SERIAL by default; `t.Parallel()` needs `arch-test:parallel-safe — <reason>`.

## Hard nos (arch-gated)
ctx-smuggled tenant (use explicit param or the tx-ctx exception only), validate-tags in domain, `Save()` on repo, business-verb repo method, public aggregate setter, mock-gen tools, `app/` importing infra. Run `task test:arch` — it catches these.

NOTE: `.claude/rules/` + `task review:tdl` are the **.NET sibling** repo's, not this Go repo.
