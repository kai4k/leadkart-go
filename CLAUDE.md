# LeadKart Go — Project Context (CLAUDE.md)

> Read this first. Then `BRD.md` (business spec) + `docs/adr/` (decisions). Rebuild plan: `.claude/plans/snazzy-strolling-cray.md`.
> **History note:** per-Wave/Phase changelog removed from this file (git holds it). Authoritative state = code + `docs/adr/` + `.claude/rules/`.

---

## What is LeadKart-Go

Go rebuild of LeadKart (.NET 10 modular monolith). Multi-tenant SaaS, Indian PCD pharma:
- Platform team sources/verifies/sells pharma leads to pharma companies.
- Tenants (pharma cos) manage leads via CRM, orders, inventory, dispatch.
- **lead** = potential buyer (medical store, chemist, distributor) — NOT a franchise partner.

8 bounded contexts: Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications.

Reference port from .NET at `d:\Development\LeadKart\`. BRD.md + .NET aggregates = spec; **Go canon drives shape** (no 1:1 translation). Sibling .NET `.claude/rules/` = canonical tiebreaker when uncertain.

## State (updated 2026-05-30)

- `main` tags: `v0.1.0` (Phase 0 foundation), `v0.2.1-phase1-identity-foundation` (Phase 1 closed, 27 tasks, ADR 0036). Phase 1.5 hardening + Waves 1-9 shipped (ADRs 0037-0058) — see git/ADRs.
- Active branch `fix/outbox-monotonic-ordering`: Watermill re-arch (ADR 0064/0067). Phase 2 (watermill-sql + Forwarder, shared `common.outbox` relay) DONE + integration-green. Phase 3 (cqrs component) in progress.
- **Up next — Phase 2 Platform module** (~5-6wk): marketplace + lead credits + verification calls. Platform-tenant detect + SuperAdmin seed already shipped (1.5).
- Branches: `main` (PR-protected prod), `archive/vibe-phase1-attempt` (reference only).

---

## Frontend topology — SvelteKit BFF + Go API (production canon)

Canonical multi-tier (Stripe/LinkedIn/Netflix/Airbnb/Walmart/PayPal/Etsy/Slack): **Node-runtime BFF wraps non-Node API**. Per ADR 0043:

```
Browser  ←─ HttpOnly cookies ─→  SvelteKit (adapter-node)  ←─ Bearer ─→  Go /api/v1/*
```

- Browser never sees raw tokens — only HttpOnly + Secure + SameSite=Lax session cookie.
- SvelteKit server runtime IS the BFF (no separate Go binary): `hooks.server.ts` reads cookie, refreshes access token, attaches `Authorization: Bearer`; `+page.server.ts` form actions + SSR loads.
- Go API stays pure bearer — no cookies, no CSRF, no HTML. Same surface serves mobile + partners.
- **Go API does NOT change for the BFF** (load-bearing): BFF is a layer above, not a refactor below. Migration = frontend-repo work.
- Deploy (v0.2-Phase 5): co-located, one front-door (Caddy/nginx/Cloudflare) TLS; `/`→SvelteKit, optional `/api/v1/*` allowlist for external bearer. Federated (mTLS) at Phase 6+.

---

## Doctrine hierarchy

| Layer | Location | Authority |
|---|---|---|
| **TDL canon** | [`docs/doctrine/tdl_canon.md`](docs/doctrine/tdl_canon.md) | THE thought process. Drift = finding. Mechanical subset enforced by `internal/architecture/tdl_strict_arch_test.go`. |
| **Living rules** | `.claude/rules/*.md` | Authoritative. Drift = finding. |
| **Architectural decisions** | `docs/adr/*.md` (Michael Nygard format) | One decision per file, dated, sealed. |
| **Long-form doctrine** | `docs/doctrine/*.md` | Detailed rule expansions. |
| **Map** | `CLAUDE.md` (this), `README.md`, `BRD.md` | Quick-start; not authoritative. |
| **Master plan** | `.claude/plans/snazzy-strolling-cray.md` (author-private) | Rebuild plan + Glossary + canonical patterns. |

**Before reviewing/extending `domain/`, `app/`, `ports/`:** read [`docs/doctrine/tdl_canon.md`](docs/doctrine/tdl_canon.md) + run `task review:tdl`. Canon doc = the *why*; arch tests block the patterns audits kept missing (ctx-smuggled tenant, validate-tags-in-domain, Save-on-repository, business-verb repo methods, mock-gen tools, setters on aggregates). Both gates exist because audits-trust-the-pattern was repeatedly wrong.

---

## Architecture — three unbreakable rules

1. Modules **NEVER** reference each other's `domain/`, `app/`, `ports/`, `adapters/` — only via Watermill integration events on the bus.
2. Each module owns its Postgres schema. No cross-schema joins.
3. `cmd/api/main.go` = thin host (Mat Ryer 2024 NewServer). Zero business logic.

**Pattern:** Modular Monolith + Hexagonal (TDL Wild Workouts) + DDD aggregates + outbox-first messaging + state-based persistence (no event sourcing at v0.1).

```
cmd/api/main.go                 # composition root — manual NewServer wiring
internal/{module}/
├── domain/                     # entities, VOs, repository interfaces
├── app/                        # command + query handlers + Application{Commands,Queries} facade
├── ports/                      # PORT — inbound HTTP/event subscribers (TDL "ports")
└── adapters/                   # ADAPTER — outbound sqlc/pgx repo, watermill publisher
```

**TDL ports/adapters** (NOT Cockburn primary/secondary):
- **port** = inbound concrete (HTTP server, event subscriber)
- **adapter** = outbound concrete (DB repo, message publisher)
- Interfaces live with their consumer (domain or app/command file), never in `ports/` or `adapters/`.

---

## Stack — locked picks

| Concern | Choice | ADR |
|---|---|---|
| Go version | 1.26.3 | 0034 |
| DB layer | sqlc (pinned v1.31.1 via `go tool`) + pgx/v5 — squirrel retired | 0004 (+Amd 1) |
| Migrations | goose | 0005 |
| Multi-tenancy | Postgres RLS (tenant data plane only) + tx-local `set_config` via Transactor | 0006 (+Amd 1) |
| HTTP | stdlib `net/http` ServeMux 1.22+ | 0007 |
| Messaging | Watermill v1.5+ + watermill-sql outbox | 0008 |
| Background jobs | river | 0010 |
| Auth | golang-jwt/jwt/v5 + refresh-token families | 0011 |
| Crypto | `golang.org/x/crypto/argon2` | 0012 |
| Logging | `log/slog` (stdlib) | 0013 |
| Observability | OpenTelemetry-Go + pprof | 0014 |
| Caching | ristretto + redis/go-redis/v9 + singleflight | 0015 |
| Real-time | coder/websocket + SSE | 0016 |
| Configuration | koanf | 0017 |
| Wiring | Manual NewServer (Mat Ryer 2024) — NO DI container | 0018 |
| Testing | stdlib `testing` + go-cmp + testify/require + testcontainers-go + goleak | 0019 |
| Validation | DDD ctor (domain) + go-playground/validator (HTTP DTO) | 0022 |
| Deployment | Chainguard distroless static + cosign + Syft + Trivy | 0024 |

**Banned:** GORM, Ent (rejected — see plan §G.D), bob, gorilla/websocket, manual `tools.go` (use `tool` directive Go 1.24+), Pike's self-referential options.

---

## Constructor patterns (TDL canon, verified Nov 2025)

| Pattern | When |
|---|---|
| `NewX(...) (*X, error)` | Universal default — invariants enforced |
| Big positional ctor `NewServer(cfg, log, app, ...)` | HTTP servers (Mat Ryer 2024) |
| Functional options | Public libraries only (DISCOURAGED for app code 2024–2026) |
| Options struct `New(cfg Config)` | App services with config |
| Aggregate factory + re-hydration: `NewTenant(...)` + `UnmarshalTenantFromDB(...)` | DDD aggregates (Wild Workouts canon) |
| Generic ctor `New[T any]` | Containers since Go 1.18 |
| `MustNewX` | init-time + tests only — ANTI-PATTERN in request paths |

---

## Testing rules (ADR 0019 + 0062)

### Test pyramid (ThreeDotsLabs canon — ADR 0062)

| Layer | Tests | How | Target per aggregate |
|---|---|---|---|
| Domain | Business rules — invariants, state machines, VO validation | Pure unit tests | Many (~10-30) |
| App / handlers | Orchestration — calls repo, emits events, error paths | Unit tests against `<aggregate>test.FakeRepository` | Many (~5-15) |
| Adapter / SQL | SQL contract — RLS fires, JSONB round-trip, 23505 translation, outbox-row write, soft-delete partial-index | Integration tests via `pgtest.RunMain` | Few (~3-6) |

### Per-aggregate fakes (mandatory)

Every domain aggregate with a `Repository` interface has a co-located `<aggregate>test/fake_repository.go` exposing `FakeRepository`:

```
internal/<module>/domain/<aggregate>/
├── repository.go                  # interface
├── role.go                        # aggregate
└── <aggregate>test/
    └── fake_repository.go         # in-memory FakeRepository
```

Constraints (enforced by `internal/architecture/tdl_canon_arch_test.go`):
- `var _ <aggregate>.Repository = (*FakeRepository)(nil)` compile-time gate
- NO `sync` imports (domain-subtree concurrency-free + single-test-owner)
- Faithful contract: mirror SQL adapter's `ErrXxx` translations, sort order, soft-delete filtering, partial-unique-index semantics

### Conventions

- Table-driven via `t.Run`. `t.Parallel()` in subtests.
- Stdlib `testing` + `go-cmp` (diffs) + `testify/require` (ergonomics).
- `testcontainers-go` for integration; build tag `//go:build integration`.
- `goleak.VerifyTestMain` (or `goleak.Find` when wrapping `pgtest.RunMain`) in integration packages.
- Shared `pgtest.Container` per package via `pgtest.RunMain`; per-test isolation via fresh `tenant.ID` + RLS (parallel) or `sharedPG.TruncateAll(t)` (cross-tenant serial).
- `t.Parallel()` + `TruncateAll(t)` in one test forbidden (`TestArch_TruncateAllImpliesSerial`).
- Race detector mandatory in CI (`go test -race -shuffle=on`).

### Integration tests SQL-contract-only (ADR 0062)

`internal/<module>/adapters/*_repository_pg_test.go` MUST test: SQLSTATE translations, RLS enforcement, JSONB round-trips, outbox-row writes, partial-index behaviors, EXPLAIN-under-RLS index selection, DB triggers.

MUST NOT exist at SQL layer (covered by `<aggregate>test.FakeRepository`): pure round-trips, ErrNotFound on missing ID, domain state machines, business-rule rejections the ctor/mutator enforces.

---

## Doctrine sources (Tier 1)

- **ThreeDotsLabs** — Watermill, Wild Workouts repo (Nov 2025 canonical), "Go with the Domain", "Go Event-Driven", threedots.tech.
- **Brandur Leach** — Crunchy Bridge architecture, sqlc/pgx at scale, river.
- **Mat Ryer** — "How I write HTTP services in Go after 13 years" (Grafana, Feb 2024).
- **Russ Cox / Ian Lance Taylor** — Go team modules + errors + generics blog.
- **Dave Cheney** — "Practical Go", error handling, "accept interfaces return structs".
- **Bryan Mills** — "Rethinking Concurrency Patterns" (GopherCon 2018).

---

## When in doubt

Trust `.claude/rules/` over this map. Trust ADRs over rules when they conflict. Trust actual code over both — if code drifts from doctrine, that's a finding: fix one or the other, never diverge silently.
