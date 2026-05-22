# Architecture Decision Records

One decision per file, Michael Nygard format (Context / Decision / Consequences / Alternatives Considered / Sources). Dated. Sealed once accepted — superseded by new ADRs, never edited in place.

## Index

| # | Title | Status |
|---|---|---|
| 0001 | Topology — Modular monolith | Accepted |
| 0002 | Architectural style — Hexagonal + DDD | Accepted |
| 0003 | Persistence default — State-based + outbox | Accepted |
| 0004 | DB layer — sqlc + pgx/v5 + squirrel + goose | Accepted |
| 0005 | Migrations — goose | Accepted |
| 0006 | Multi-tenancy — Postgres RLS + SET LOCAL | Accepted |
| 0007 | HTTP router — stdlib `net/http` ServeMux 1.22+ | Accepted |
| 0008 | Messaging — Watermill v1.5+ + watermill-sql outbox | Accepted |
| 0009 | Command dispatch — Application{Commands, Queries} facade | Accepted |
| 0010 | Background jobs — river | Accepted |
| 0027 | Audit log — outbox doubles as audit | Accepted |
| 0028 | SecurityStampCache + stale-write fence | Accepted |
| 0029 | Two-binary deploy: cmd/api + cmd/worker | Accepted |
| 0030 | Canonical public-API HTTP middleware chain | Accepted |
| 0031 | HTTP idempotency via `X-Command-Id` | Accepted |
| 0033 | Tenant context — `tenant.FromContext(ctx)` package func | Accepted |
| 0034 | Go version — 1.26.2 (post Phase 1 dep-bump) | Accepted |
| 0035 | Event sourcing scope — zero modules at v0.1 | Accepted |
| 0036 | Permission model — closed-set catalog + Role + per-Membership overlay | Accepted |
| 0037 | sqlc generated-code package layout — dedicated `db` subpackage | Accepted |
| 0038 | Pagination strategy — cursor (keyset) over offset | Accepted |
| 0039 | Per-request scope selection — JWT.is_platform + X-Tenant-Id header | Accepted |
| 0040 | Search strategy — `pg_trgm` now, Postgres FTS at Phase 4, defer dedicated infra | Accepted |
| 0041 | CQRS read models via outbox subscribers | Accepted |
| 0042 | Cache TTL strategy — per-use-case profiles with jitter | Accepted |
| 0043 | Frontend topology — SvelteKit BFF (adapter-node) + Go API | Accepted |
| 0044 | Enumeration safety — 404 on no-access for guessable identifiers | Accepted |
| 0045 | Scoped JWT impersonation — AWS STS AssumeRole + RFC 8693 `act` claim (design; impl in Wave 4) | Accepted |
| 0046 | OpenAPI spec-first contract + Scalar `/docs` | Accepted |
| 0047 | Layer-boundary discipline — `app/` cannot import DB driver / sqlc / concrete adapter | Accepted |

ADRs 0011–0026 + 0032 + 0048+ land as the relevant code lands per the master plan.
