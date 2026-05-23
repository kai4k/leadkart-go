# LeadKart — Go Edition

Go rebuild of LeadKart (multi-tenant Indian PCD pharma SaaS). Reference port from the .NET 10 implementation; architecture derived from 2026 Go canon (ThreeDotsLabs DDD/EDA, Mat Ryer HTTP services, Brandur Leach sqlc/pgx) — **not** a 1:1 .NET translation.

## Status

**Phase 1.5 closed + Waves 1–9 shipped.** The Identity bounded context runs the full surface: register-tenant + login (with NIST-aligned account lockout) + refresh + logout, password + email change flows, 60+ HTTP routes, scoped-JWT impersonation (RFC 8693), role hierarchy + permission-elevation approval workflows, OpenAPI 3.1 spec as code-of-record with `Spectral` lint + drift gate, layer-boundary arch tests, EDA-driven email + audit-log enrichment.

For the wave-by-wave delivery history (Phase 1.5 + Waves 1–9), see [`CLAUDE.md`](CLAUDE.md) § *Current state*. For architecture decisions, see [`docs/adr/`](docs/adr/) (57 ADRs, Michael Nygard format).

**Next:** Phase 2 — Platform bounded context (marketplace + lead credits + verification calls). See [`BRD.md`](BRD.md) + the master plan.

## Stack

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.26+ | ADR 0034 |
| Architecture | Modular monolith + Hexagonal (TDL Wild Workouts canon) | ADR 0001, 0002 |
| Persistence | Postgres + sqlc + pgx/v5 + squirrel | ADR 0004 |
| Migrations | goose | ADR 0005 |
| Multi-tenancy | Postgres RLS + `SET LOCAL app.tenant_id` via pgxpool `AfterAcquire` | ADR 0006 |
| HTTP | stdlib `net/http` ServeMux 1.22+ | ADR 0007 |
| Messaging | Watermill v1.5+ in-proc + watermill-sql outbox | ADR 0008 |
| Background jobs | river (Postgres-backed) | ADR 0010 |
| Auth | golang-jwt/jwt/v5 + refresh-token families + RFC 8693 scoped-JWT impersonation | ADRs 0011, 0045 |
| Crypto | `golang.org/x/crypto/argon2` | ADR 0012 |
| Logging | `log/slog` (stdlib) | ADR 0013 |
| Observability | OpenTelemetry-Go + pprof | ADR 0014 |
| Caching | ristretto (L1) + redis/go-redis/v9 (L2) + singleflight | ADRs 0015, 0042 |
| Real-time | coder/websocket + SSE | ADR 0016 |
| Configuration | koanf | ADR 0017 |
| Wiring | Manual `NewServer` constructor (Mat Ryer 2024) | ADR 0018 |
| Testing | stdlib `testing` + go-cmp + testify/require + testcontainers-go + goleak | ADR 0019 |
| Linting | golangci-lint v2 strict | ADR 0020 |
| Vuln scan | govulncheck | ADR 0021 |
| Validation | DDD constructors (domain) + go-playground/validator (HTTP DTO) | ADR 0022 |
| API contract | OpenAPI 3.1 spec-first + Spectral lint + drift gate (`Scalar` UI at `/docs`) | ADRs 0046, 0050 |
| Deployment | Chainguard distroless static binary + cosign + Syft + Trivy | ADR 0024 |

## Local development

Requirements: Go 1.26+, Docker Desktop, [Task](https://taskfile.dev/installation/), [Node 22+](https://nodejs.org/) (for `task ci:openapi` Spectral lint), [Air](https://github.com/air-verse/air) for hot reload.

```bash
# Start Postgres + Redis
docker compose -f docker/compose.yml up -d

# Apply migrations
task migrate:up

# Provision platform tenant + SuperAdmin (idempotent; reads LEADKART_SUPERADMIN__* env)
task bootstrap

# Run all unit + arch tests
task test
task test:arch

# OpenAPI spec lint
task ci:openapi

# Full pre-push gate (vet + lint + test + arch + integration-compile + vuln + build)
task ci

# Run API with hot reload on :8080
task run

# Bare http://localhost:8080/ redirects to /docs (Scalar UI). The OpenAPI spec
# lives at /openapi.yaml. Probes (/alive, /ready, /health) are on the admin
# listener at :9090 — never on the public listener (audit-checklist.md §12).
```

## Repo layout

```
cmd/                                  # one main per process (per ADR 0029)
├── api/                              # HTTP API binary (request path only)
├── worker/                           # outbox forwarder + Watermill subscribers + jobs
├── migrate/                          # goose migration runner
└── bootstrap/                        # platform-tenant + SuperAdmin seed CLI

internal/                             # all business code (Go internal/ rule)
├── common/                           # shared kernel — every module imports these:
│                                     #   audit, cache, clock, config, email, errs, httpmw,
│                                     #   idempotency, ids, jobs, messaging, obs, openapi,
│                                     #   pagination, pg, slug, tenancy, druglicence, gst,
│                                     #   pan, phone, postaladdress  (per ADR 0048 — merged
│                                     #   from the previous common/ + platform/ split)
└── identity/                         # bounded context — the first canonical module
    ├── domain/                       # aggregates (tenant, person, membership, role,
    │                                 # refreshtoken, permissionrequest, impersonation,
    │                                 # passwordpolicy), value objects, repository interfaces
    ├── app/                          # CQRS handlers + Application{Commands,Queries} facade
    │   ├── command/                  # write handlers
    │   ├── query/                    # read handlers
    │   └── arch_test.go              # boundary discipline gate (ADR 0047)
    ├── ports/                        # PORT (inbound) — HTTP handlers, Watermill subscribers
    │   ├── subscribers/              # cache-evict, family-revoke, email-send, SIEM
    │   └── route_spec_test.go        # spec/code drift gate (ADR 0050)
    └── adapters/                     # ADAPTER (outbound) — sqlc/pgx repos, in-memory stores,
                                      # outbox writer, search index, audit + stats readers

api/openapi.yaml                      # canonical HTTP contract (Stripe/GitHub/Anthropic canon)
migrations/                           # goose .sql migrations (timestamp-prefixed)
docker/                               # Dockerfile (multi-target) + compose.yml + smoke
docs/adr/                             # 57 ADRs (Michael Nygard) — auth, EDA, arch, CI gates
.spectral.yaml                        # OpenAPI lint ruleset (extends spectral:oas + LeadKart)
.github/workflows/ci.yml              # paths-filter-gated pipeline (changes → {unit, arch,
                                      # integration, migrations, openapi, docker, smoke})
```

Per ThreeDotsLabs' explicit teaching: *port = inbound concrete impl* (HTTP/gRPC server, message subscriber), *adapter = outbound concrete impl* (DB repository, message publisher). **Not the textbook Cockburn vocabulary** ("primary port", "secondary adapter") — TDL deliberately collapsed those.

## License

Proprietary (LeadKart). All rights reserved.
