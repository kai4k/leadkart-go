# LeadKart Go — Project Context (CLAUDE.md)

> Read this file first. Then `BRD.md` (business spec) and `docs/adr/` (architecture decisions). The full rebuild plan lives at `.claude/plans/snazzy-strolling-cray.md` (author tooling).

---

## What is LeadKart-Go

Go rebuild of LeadKart (.NET 10 modular monolith). Multi-tenant SaaS for Indian PCD pharma:

- LeadKart platform team sources, verifies, and sells pharma leads to pharma companies.
- Tenants (pharma companies) manage those leads through CRM, orders, inventory, dispatch.
- A **lead** is a potential buyer (medical store, chemist, distributor) — NOT a franchise partner.

8 bounded contexts: Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications.

Reference port from the .NET implementation at `d:\Development\LeadKart\`. BRD.md + .NET aggregates are the spec; **Go canon drives shape** (no 1:1 translation).

---

## Doctrine hierarchy

| Layer | Location | Authority |
|---|---|---|
| **Living rules** | `.claude/rules/*.md` | Authoritative. Drift = finding. |
| **Architectural decisions** | `docs/adr/*.md` (Michael Nygard format) | One decision per file, dated, sealed. |
| **Long-form doctrine** | `docs/doctrine/*.md` | Detailed rule expansions (TBD as code lands). |
| **Map** | `CLAUDE.md` (this), `README.md`, `BRD.md` | Quick-start; not authoritative. |
| **Master plan** | `.claude/plans/snazzy-strolling-cray.md` (author-private) | Rebuild plan + Glossary + canonical patterns. |

---

## Architecture — three unbreakable rules

1. Modules **NEVER** reference each other's `domain/`, `app/`, `ports/`, `adapters/` — only via Watermill integration events on the bus.
2. Each module owns its Postgres schema. No cross-schema joins.
3. `cmd/api/main.go` is a thin host (Mat Ryer 2024 NewServer pattern). Zero business logic.

**Pattern:** Modular Monolith + Hexagonal (TDL Wild Workouts canon) + DDD aggregates + outbox-first messaging + state-based persistence (no event sourcing at v0.1).

```
cmd/api/main.go                 # composition root — manual NewServer wiring
internal/{module}/
├── domain/                     # entities, VOs, repository interfaces
├── app/                        # command + query handlers + Application{Commands,Queries} facade
├── ports/                      # PORT — inbound HTTP/event subscribers (TDL "ports")
└── adapters/                   # ADAPTER — outbound sqlc/pgx repo, watermill publisher
```

**TDL ports/adapters terminology** (deliberately NOT Cockburn's primary/secondary):
- **port** = inbound concrete impl (HTTP server, event subscriber)
- **adapter** = outbound concrete impl (DB repo, message publisher)
- Interfaces live with their consumer (domain or app/command file), never in `ports/` or `adapters/`.

---

## Stack — locked picks

| Concern | Choice | ADR |
|---|---|---|
| Go version | 1.25 (target 1.26+) | 0034 |
| DB layer | sqlc + pgx/v5 + squirrel | 0004 |
| Migrations | goose | 0005 |
| Multi-tenancy | Postgres RLS + SET LOCAL via pgxpool AfterAcquire | 0006 |
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

**Banned:** GORM, Ent (rejected after deep validation — see plan §G.D), bob, gorilla/websocket, manual `tools.go` (use `tool` directive Go 1.24+), Pike's self-referential options.

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

## Testing rules (per ADR 0019)

- Table-driven via `t.Run`. `t.Parallel()` in subtests.
- Stdlib `testing` + `go-cmp` for diffs + `testify/require` for ergonomics.
- `testcontainers-go` for integration tests; build tag `//go:build integration`.
- `goleak.VerifyTestMain` in integration packages.
- Fakes per module under `{module}/{module}test/`.
- Race detector mandatory in CI (`go test -race -shuffle=on`).

---

## Doctrine sources (Tier 1 — current)

- **ThreeDotsLabs** — Watermill, Wild Workouts repo (Nov 2025 canonical), "Go with the Domain", "Go Event-Driven" training, blog (threedots.tech).
- **Brandur Leach** — Crunchy Bridge architecture, sqlc/pgx production at scale, river queue.
- **Mat Ryer** — "How I write HTTP services in Go after 13 years" (Grafana, Feb 2024).
- **Russ Cox / Ian Lance Taylor** — Go team modules + errors + generics blog posts.
- **Dave Cheney** — "Practical Go", error handling, "accept interfaces return structs".
- **Bryan Mills** — "Rethinking Concurrency Patterns" (GopherCon 2018, still canon).

---

## When in doubt

Trust `.claude/rules/` over this map. Trust ADRs over rules when they conflict. Trust the actual code over both — and if the code drifts from doctrine, that's a finding: fix one or the other, never let them diverge silently.
