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

ADRs 0011–0026 + 0032 + 0038+ land as the relevant code lands per the master plan.
