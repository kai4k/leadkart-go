# LeadKart — Go Edition

Go rebuild of LeadKart (multi-tenant Indian PCD pharma SaaS). Reference port from the .NET 10 implementation; architecture derived from 2026 Go canon (Three Dots Labs DDD/EDA, Mat Ryer HTTP services, Brandur Leach sqlc/pgx) — **not** a 1:1 .NET translation.

## Status

**v0.1 in active scaffolding** — Identity module is the first canonical bounded context.

See [`docs/adr/`](docs/adr/) for architectural decisions and [`.claude/plans/`](https://github.com/anrgchauhan/.claude/blob/main/plans/) for the master rebuild plan (private to author tooling).

## Stack

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.25 (target 1.26+) | Plan ADR 0034 |
| Architecture | Modular monolith + Hexagonal (TDL Wild Workouts canon) | ADR 0001, 0002 |
| Persistence | Postgres + sqlc + pgx/v5 + squirrel | ADR 0004 |
| Migrations | goose | ADR 0005 |
| Multi-tenancy | Postgres RLS + `SET LOCAL app.tenant_id` via pgxpool `AfterAcquire` | ADR 0006 |
| HTTP | stdlib `net/http` ServeMux 1.22+ | ADR 0007 |
| Messaging | Watermill v1.5+ in-proc + watermill-sql outbox | ADR 0008 |
| Background jobs | river (Postgres-backed) | ADR 0010 |
| Auth | golang-jwt/jwt/v5 + refresh-token families | ADR 0011 |
| Crypto | `golang.org/x/crypto/argon2` | ADR 0012 |
| Logging | `log/slog` (stdlib) | ADR 0013 |
| Observability | OpenTelemetry-Go + pprof | ADR 0014 |
| Caching | ristretto (L1) + redis/go-redis/v9 (L2) + singleflight | ADR 0015 |
| Real-time | coder/websocket + SSE | ADR 0016 |
| Configuration | koanf | ADR 0017 |
| Wiring | Manual `NewServer` constructor (Mat Ryer 2024) | ADR 0018 |
| Testing | stdlib `testing` + go-cmp + testify/require + testcontainers-go + goleak | ADR 0019 |
| Linting | golangci-lint v2 strict | ADR 0020 |
| Vuln scan | govulncheck | ADR 0021 |
| Validation | DDD constructors (domain) + go-playground/validator (HTTP DTO) | ADR 0022 |
| Deployment | distroless static binary + cosign + Syft + Trivy | ADR 0024 |

## Local development

Requirements: Go 1.25+, Docker Desktop, [Task](https://taskfile.dev/installation/), [Air](https://github.com/air-verse/air) for hot reload.

```bash
# Start Postgres + Redis
docker compose -f docker/compose.yml up -d

# Run all tests (race detector requires CGO; CI runs with -race)
task test

# Run API with hot reload on :8080
task run
# Or directly: go run ./cmd/api

# Health check
curl localhost:8080/health  # → ok
```

## Repo layout (TDL Wild Workouts canonical, verified Nov 2025)

```
cmd/                                # one main per process
├── api/                            # HTTP API binary
├── worker/                         # river background worker (when added)
└── migrate/                        # goose migration runner (when added)

internal/                           # all business code (Go internal/ rule)
├── platform/                       # composition-root helpers (db, http, obs, auth)
├── common/                         # shared kernel (ids, errs, tenancy, clock, money, pii)
└── identity/                       # bounded context — first canonical module
    ├── domain/                     # aggregates, VOs, repository interfaces
    ├── app/{app.go, command/, query/}  # CQRS handlers + Application{Commands, Queries} facade
    ├── ports/                      # PORT (inbound) — HTTP handlers, event subscribers
    └── adapters/                   # ADAPTER (outbound) — sqlc/pgx repo, outbox writer, Watermill publisher

api/openapi.yaml                    # single source of truth for HTTP contract
migrations/                         # goose .sql migrations (timestamp-prefixed)
docker/                             # Dockerfile (multi-target) + compose.yml
docs/{adr,doctrine}/                # ADRs (Michael Nygard) + long-form rule docs
.claude/rules/                      # AI-assistant primer; references ADRs + doctrine
```

Per Three Dots Labs' explicit teaching: *port = inbound concrete impl* (HTTP/gRPC server), *adapter = outbound concrete impl* (DB, message publisher). **Not the textbook Cockburn vocabulary** ("primary port", "secondary adapter") — TDL deliberately collapsed those.

## License

Proprietary (LeadKart). All rights reserved.
