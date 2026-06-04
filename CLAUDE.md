# LeadKart Go — Project Context (CLAUDE.md)

> Map only — not authoritative. Authoritative = code + `docs/adr/` + `docs/doctrine/tdl_canon.md`. Git holds history.
> Touching `domain/`/`app/`/`ports/`? Invoke the **tdl-discipline** skill first. Before claiming green/committing, the **verify-green** skill.

## What is LeadKart-Go

Go rebuild of LeadKart (.NET 10 modular monolith). Multi-tenant SaaS, Indian PCD pharma:
- Platform team sources/verifies/sells pharma leads to pharma companies.
- Tenants (pharma cos) manage leads via CRM, orders, inventory, dispatch.
- **lead** = potential buyer (medical store, chemist, distributor), NOT a franchise partner.

8 bounded contexts: Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications.
.NET reference at `d:\Development\LeadKart\` (spec, not 1:1 — Go canon drives shape).

## Architecture — three unbreakable rules

1. Modules NEVER import each other's `domain/app/ports/adapters` — only via Watermill integration events on the bus.
2. Each module owns its Postgres schema. No cross-schema joins.
3. `cmd/api/main.go` = thin host (Mat Ryer 2024 NewServer). Zero business logic.

Pattern: Modular Monolith + Hexagonal (TDL Wild Workouts) + DDD aggregates + outbox-first messaging + state-based persistence (no event sourcing).

```
cmd/api/main.go            # composition root — manual NewServer wiring
internal/{module}/
├── domain/                # entities, VOs, repository interfaces
├── app/                   # command/query handlers + Application{Commands,Queries} facade
├── ports/                 # inbound concrete: HTTP servers, event subscribers
└── adapters/              # outbound concrete: sqlc/pgx repos, watermill publishers
```
TDL ports/adapters (NOT Cockburn). Interfaces live with their consumer (domain or app), never in ports/ or adapters/.

## Stack — locked picks

| Concern | Choice | ADR |
|---|---|---|
| Go | 1.26.4 | 0034 |
| DB | sqlc (pinned v1.31.1) + pgx/v5 (squirrel retired) | 0004 |
| Migrations | goose | 0005 |
| Multi-tenancy | Postgres RLS (tenant plane only) + tx-local set_config via Transactor | 0006 |
| HTTP | stdlib net/http ServeMux 1.22+ | 0007 |
| Messaging | Watermill v1.5 cqrs (per-tx EventBus + EventProcessor) + watermill-sql outbox + Forwarder; at-least-once + idempotent handlers | 0008, 0064, 0067 |
| Background jobs | river | 0010 |
| Auth | golang-jwt/jwt/v5 + refresh-token families | 0011 |
| Crypto | x/crypto/argon2 | 0012 |
| Logging | log/slog | 0013 |
| Observability | OpenTelemetry-Go + pprof | 0014 |
| Caching | ristretto + redis/go-redis/v9 + singleflight | 0015 |
| Real-time | coder/websocket + SSE | 0016 |
| Config | koanf | 0017 |
| Wiring | manual NewServer — NO DI container | 0018 |
| Testing | stdlib testing + go-cmp + testify/require + testcontainers-go + goleak | 0019, 0062 |
| Validation | DDD ctor (domain) + go-playground/validator (HTTP DTO) | 0022 |
| Deploy | Chainguard distroless + cosign + Syft + Trivy | 0024 |
| Frontend | SvelteKit BFF (adapter-node, HttpOnly cookies) wraps pure-bearer Go API | 0043 |

Banned: GORM, Ent, bob, gorilla/websocket, manual `tools.go` (use `tool` directive), Pike self-referential options.

## Doctrine hierarchy (authority order)

1. `docs/doctrine/tdl_canon.md` — THE thought process. Drift = finding. Mechanical subset gated by `internal/architecture/tdl_strict_arch_test.go`.
2. `docs/adr/*.md` — sealed decisions (Nygard). ADRs override the map. Superseded → `docs/adr/archive/`.
3. This map + README + BRD — quick-start only.

Trust ADRs over this map; trust actual code over both — code drift from doctrine is a finding, fix one side, never diverge silently.

## Gates + skills
- `task test:arch` runs `TestArch_*`/`TestMeta_*` — blocks the patterns audits miss (ctx-smuggled tenant, validate-tags-in-domain, Save-on-repo, business-verb repo methods, mock-gen, aggregate setters).
- **tdl-discipline** skill = the operational TDL checklist; **verify-green** skill = gate-before-commit. Hooks: gofmt-on-write, SessionStart orient, Stop build+arch warn.

## Doctrine sources (Tier 1)
ThreeDotsLabs (Watermill, Wild Workouts, Go Event-Driven), Brandur Leach (pgx/sqlc/river at scale), Mat Ryer (HTTP services 2024), Russ Cox/Ian Lance Taylor (Go team), Dave Cheney (Practical Go), Bryan Mills (concurrency).
