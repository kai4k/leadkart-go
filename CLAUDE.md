# LeadKart Go — Project Context (CLAUDE.md)

> Read this file first. Then `BRD.md` (business spec) and `docs/adr/` (architecture decisions). The full rebuild plan lives at `.claude/plans/snazzy-strolling-cray.md` (author tooling).

---

## Current state (updated 2026-05-10)

**Tagged releases on `main`:**
- `v0.1.0` — Phase 0 / foundation baseline. Identity domain layer + sqlc adapters + JWT issuer + outbox forwarder.
- `v0.2.1-phase1-identity-foundation` — Phase 1 closed. 27 tasks shipped:
  - Permission package (closed-set catalog + Flyweight intern)
  - Role aggregate (Tenant-scoped, system-default + super-admin flags, soft-delete)
  - Membership extensions (RoleAssignments, permission overlay, profile, manager hierarchy)
  - Migration 20260507000002 — `roles` + `role_assignments` + `permission_overrides` + `tenant_memberships` ALTER, all RLS+FORCE
  - Repositories: RoleRepository (Add/UpdateByID/GetByIDs/ListByTenant) + MembershipRepository (extended for role+overlay+profile)
  - DefaultRoleCatalog + ApplyDefaultRoles seed; TenantOnboardingService auto-assigns CompanyOwner with `Meta.TenantAdmin`
  - PermissionResolver + ResolveAuth (perms + IsSuperUser bundle)
  - Login/Refresh JWT permission-claim wiring
  - HTTP middleware: `authn.RequirePermission` / `RequireAnyPermission` / `RequirePlatform`
  - 11 V1 integration events (Role* + Membership* lifecycle)
  - E2E gate test (login → JWT → middleware-gated handler)
  - ADR 0036 — Permission model
- Audit sweep: Go 1.26.2 + golangci-lint v2.12.2 + 31 anti-pattern fixes (modern stdlib idioms + LeadKart doctrine bans).

**Phase 1.5 hardening shipped (post-Phase-1, pre-Phase-2; merged via PRs through 2026-05-10):**
- `cmd/bootstrap` CLI — env-driven SuperAdmin + platform-tenant seed (Stripe/Plaid canon: data seeding lives outside migrations). Idempotent ON CONFLICT.
- Single-JOIN auth-routing in login (Brandon Mitchell / DHH 2024-2026 canon over the legacy denormalised auth_routing-table approach). Saves one network roundtrip per login.
- Migration 20260507000007 — per-caller idempotency scoping.
- Migration 20260507000008 — DROP `tenants.admin_email` (denormalised, never auto-synced); ADD `tenant_memberships.created_by_membership_id` + `persons.created_by_person_id` audit-chain columns.
- `command.ErrPlatformTenantUndeletable` — Suspend/MarkForDeletion/HardDelete refuse tenants holding any active SuperAdmin role.
- `authn.RequirePlatform` slug-anchored — defense-in-depth check that JWT `is_platform=true` + `tenant_slug == "platform"` agree.
- `authn.PlatformTenantSlug` constant + Login mints `is_platform` from `tenant.Slug() == "platform"`.
- CI optimisation: single workflow + `dorny/paths-filter@v3`, pre-push hook (`task ci`), Taskfile `task dev` + `task hooks:install`.
- sqlc layout — generated code split into `internal/identity/adapters/db/` (`package db`); hand-written `*_pg.go` adapters import + qualify (`db.Queries`, `db.IdentityPerson`). Brandur / river-queue canon. ADR 0037.
- Pagination + search + cache foundation (Wave 1 of the frontend backend wishlist):
  - ADR 0038 — cursor (keyset) pagination over offset; opaque base64 tokens; `has_more`/`next_cursor`; no total counts.
  - ADR 0039 — per-request scope selection (JWT.is_platform + `X-Tenant-Id` header decision tree) for the unified-surface spec.
  - ADR 0040 — search strategy: `pg_trgm` now, Postgres FTS at Phase 4, defer dedicated infrastructure.
  - ADR 0041 — CQRS read models via outbox subscribers; pattern for Phase 2 lead-search projection.
  - Migration 20260518000001 — `pg_trgm` extension + GIN search indexes on persons/tenants/memberships + composite keyset indexes for pagination + partial-index sweep.
  - `internal/common/pagination` — generic `Page[T]`, `Cursor` opaque base64 token, `ClampPageSize` helper. Zero project-internal imports.
  - `GET /v1/auth/me/capabilities` — JWT-resident projection (no DB hit) so the frontend stops decoding tokens to drive nav/tier/buttons. Auth0 /userinfo canon.
  - `GET /v1/platform/stats?delta_window=24h|7d|30d` — closed-set delta window per ADR 0040 (cache-key-explosion prevention); cache wrapping via HybridCache facade ready to wire.
  - `GET /v1/users` cursor-paginated — `?cursor=&page_size=` (default 50, cap 200). Backed by new `ListActiveMembershipsInTenantPage` sqlc query + composite partial index. Active-only; inactive listing is a follow-up `?status=inactive` endpoint.
- Wave 2 — cache wiring + omni-search + audit-log reads + EXPLAIN test:
  - ADR 0042 — cache TTL strategy. Five profiles (Default / SecurityStamp / Capabilities / SearchResults / Dashboard) with research-grounded TTLs + jitter discipline (±10% on dashboard + search). Microsoft HybridCache canon.
  - `HybridCache` facade wired for `/v1/platform/stats` (DashboardTTL — 1min L1 / 5min L2 + jitter) and `/v1/auth/me/capabilities` profile enrichment (CapabilitiesTTL — 2min L1 / 15min L2; security_stamp keyed for implicit invalidation).
  - `GET /v1/search` omni-search — parallel pg_trgm fanout (persons + tenants) with per-category timeout + `has_partial` flag. Platform-only. Cached via SearchResultsTTL.
  - `GET /v1/auth/me/activity` + `GET /v1/tenants/{tenantId}/audit/events` — keyset-paginated audit-log reads against `buildingblocks.audit_log_entry`. Self-read always allowed; tenant-scoped goes through `RequireTenantContext`. Sub-resource path (`/audit/events`) avoids Go 1.22 ServeMux conflict with `/tenants/by-slug/{slug}` per Wave 7 hotfix.
  - EXPLAIN-under-RLS integration test (`keyset_explain_integration_test.go`) — load 200 memberships, assert keyset query uses `idx_memberships_tenant_active_joined` (Index Scan, not Seq Scan). ADR 0038 discipline as a CI gate.
- ADR 0043 — frontend topology target: SvelteKit BFF (adapter-node) + Go API. Canonical for production-scale orgs (Stripe / LinkedIn / Netflix / Airbnb / Walmart / PayPal / Etsy / Slack). Browser holds HttpOnly cookie; SvelteKit server runtime IS the BFF; Go API stays pure bearer. **Zero Go-side code changes** — the BFF migration is entirely frontend-repo work.
- Wave 3 — slug/email lookup hardening + RFC 9457 errors + migration gate + scoped-JWT design:
  - ADR 0044 — Enumeration safety. 404 (not 403) on no-access for guessable identifiers (slugs / emails / handles). GitHub / Stripe / Auth0 / Twilio canon; OWASP API Top 10 §A01:2023 anti-pattern when 403 leaks existence.
  - `GET /v1/tenants/by-slug/{slug}` with handler-inline authz + enumeration-safe 404 + byte-equality test (`TestE2E_TenantBySlug_ResponseShapesIdentical` proves cross-tenant 404 ≡ missing-slug 404).
  - `GET /v1/platform/persons?email=` — Platform-only cross-tenant identity probe by email (Stripe / Auth0 canon — query param, not path).
  - RFC 9457 Problem Details error shape — `ErrorResponse` now carries `type`/`title`/`status`/`detail`/`errors{}` per the spec while keeping legacy `error`/`message` for backward compat. `writeValidationError` helper for field-level validation rejection (422 + `errors: {field: [msgs]}`).
  - A.8 `writeMutationResult` helper — 200 + DTO when supplied, 204 when nil. Per-handler adoption is incremental + non-breaking.
  - Migration CI gate — new `task ci:migrations` (local) + GitHub Actions `migrations-check` job (cloud) applies all migrations to ephemeral Postgres on every PR touching `migrations/`. Catches the GIN-on-uuid bug class permanently.
  - ADR 0045 — Scoped JWT impersonation design (companion to Wave 4 impl). AWS STS AssumeRole pattern + RFC 8693 `act` claim + downgraded scope + `aud: "impersonation"` discrimination + actor-chain audit-log columns.
- Wave 4 — scoped JWT impersonation IMPLEMENTATION (ADR 0045):
  - Migration 20260524000001 — `audit_log_entry` gains `act_operator_id` + `act_session_id` + `act_reason` nullable columns + partial indexes for forensic queries.
  - `jwt.Claims` gains `Act *ActClaim` (RFC 8693 §4.1); `jwt.Issuer.Issue` accepts `Audience` override + `TTL` override + `Act` for the impersonation path; `Verify` accepts multi-audience closed set (`AudienceClaim` + `ImpersonationAudienceClaim`).
  - `CreateImpersonationSessionHandler` extended — resolves target tenant, mints scoped JWT with `is_platform=false` + `is_super_user=false` (DOWNGRADED), `permissions=[Meta.TenantAdmin]`, `aud="leadkart-impersonation"`, TTL = session lifetime. Returns `access_token` in the 201 response.
  - `CreateImpersonationSessionResponse` DTO gains `access_token` + `access_token_expires_at_utc` + `token_type`.
  - Synthetic membership_id derived deterministically from session_id (SHA-256 truncated, v4-shaped). Handlers expecting a real membership row get ErrNotFound; tolerated per ADR 0045.
  - No refresh-token-for-impersonation in v0.2 — AWS STS canon: re-AssumeRole if you need longer than the session. Reduces Wave 4 scope ~1 day; can layer on if measured pain.
  - Audit-log enrichment (writing the new act_* columns) deferred to Wave 4.1 — **shipped in Wave 9.2c per ADR 0056** (act-claim propagation through ctx → outbox columns → Watermill message.Metadata → AuditLoggingMiddleware → audit_log_entry.act_*).
  - E2E integration tests covering: scoped-token issuance + claim shape verification + downgraded-scope blocks `/v1/platform/*` + sub-impersonation rejected + target-not-found → 404.
- Wave 5 — OpenAPI 3.1 spec-first contract + Scalar `/docs`:
  - ADR 0046 — spec-first over code-first (Stripe / GitHub / Anthropic canon). Hand-written `api/openapi.yaml` is the canonical contract; Go handlers conform to it. Scalar UI over Swagger UI / Redoc (Anthropic / Resend / Hono use Scalar).
  - `api/openapi.yaml` — ~50 operations covering Auth / Capabilities / Sessions / Tenants / Users / Roles / Platform / Search / Audit. Versioned `info.version: 0.2.0`; tags reflect URL groups.
  - `internal/common/openapi/` package — `//go:embed all_routes.yaml` makes the spec a build-baked binary asset (ADR 0024 distroless static fit; no external file dependency). `SpecHandler()` serves the YAML at `GET /openapi.yaml`; `ScalarHandler()` serves the Scalar UI HTML at `GET /docs` + `GET /docs/`.
  - Root `GET /` now 302-redirects to `/docs` so bare `localhost:8080` lands engineers + PMs on the interactive docs instead of a JSON-pointing-elsewhere body.
  - Frontend now runs `openapi-typescript` against `/openapi.yaml` → fully-typed TS clients instead of hand-maintained DTO interfaces (the Wave 5 frontend-pain motivator).
  - Drift-prevention follow-ups — `task ci:openapi` (Spectral lint) + `TestArch_RouteHasSpecOperation` (Go arch test asserting every `mux.Handle` has a matching operation in the spec). **Shipped in Wave 9.3 per ADR 0050.**
- Wave 6 — Layer-boundary discipline + CI gate (ADR 0047):
  - ADR 0047 — `app/` may NOT import `adapters/db` (sqlc-generated rows), `jackc/pgx/v5` / `pgxpool` / `pgtype` (driver), or `internal/identity/adapters` (concrete repository structs). TDL Wild Workouts canon + Cheney "accept interfaces, return structs" + Khorikov pragmatic clean-architecture + Brandur ctx-tx pattern.
  - Read-side interfaces — `audit.Reader` (in `internal/common/audit/`), `query.SearchIndex` + `query.PlatformStatsReader` (in `internal/identity/app/query/`). Concrete pg-backed impls live in `adapters/` (`AuditReaderPG`, `SearchIndexPG`, `PlatformStatsReaderPG`).
  - `pg.UnitOfWork` interface for multi-aggregate same-tx writes. The active `pgx.Tx` is stashed in ctx via `pg.contextWithTx` (unexported) + retrieved by adapter code via `pg.TxFromContext(ctx)`. Handlers (`RegisterTenant`, `CreateUser`) depend ONLY on `pg.UnitOfWork` + domain repository interfaces — no pgx, no concrete adapters.
  - Adapter `Add(ctx, agg)` methods now check `pg.TxFromContext(ctx)` — if a parent UoW is in flight, join its tx; otherwise open own. Canonical `addOnTx` unexported helper replaces the previous exported `AddInTx`.
  - `Transactor.WithinTx` renamed to `WithinTxPgx` for the low-level adapter-facing variant; new `WithinTx(ctx, scope, fn func(ctx) error)` on `*Transactor` is the UoW-shaped boundary-clean variant.
  - `TestArch_AppDoesNotImportForbidden` — arch test in `internal/identity/app/` walks every non-test `.go` file under `app/` and fails CI on any import of the forbidden list. Drift becomes impossible at PR time. `task test:arch` runs it alongside the integration-event arch tests.
- Wave 7 — `internal/platform/` merged into `internal/common/` (ADR 0048):
  - TDL strict-canon alignment. TDL Wild Workouts (named Tier 1 reference in CLAUDE.md) uses ONE shared root: `internal/common/`. The previous two-tier split (`common/` pure + `platform/` infra) was a LeadKart-Go invention; the merge brings the layout in line with TDL + Microsoft eShop + Vernon IDDD canon (all use single "BuildingBlocks" root).
  - 13 sub-packages moved: `audit, breach, cache, config, email, httpmw, idempotency, impersonation, jobs, messaging, obs, openapi, pg` → `internal/common/`. Email merged with the existing `common/email` VO package (`ErrInvalid` → Address VO; new `ErrInvalidMessage` → gateway Message). **Wave 9.1a+b update (ADR 0051):** `breach` + `impersonation` subsequently moved OUT of `internal/common/` into `internal/identity/{domain,adapters}/` per single-module rule — they were Identity-only and didn't belong in the shared kernel.
  - `internal/platform/` namespace now AVAILABLE for the Phase 2 Platform bounded context (marketplace + lead credits + verification calls). No more name collision.
  - Boundary enforcement unchanged — `TestArch_AppDoesNotImportForbidden` (ADR 0047) bans substrate imports by import path, not by folder name; merge is a no-op for the gate.
- Wave 8 — URL design rules + route-registration arch gate + integration-tag compile gate (ADR 0049):
  - `TestArch_RouteRegistration_NoConflicts` in `internal/identity/ports/route_registration_test.go` — wires `ports.AddRoutes` against a fresh `http.ServeMux` with stub verifier + stamp-validator; fails on any Go 1.22+ pattern-overlap panic. Catches the bug class that broke Wave 7 (activity vs by-slug) at unit-test time, NOT at remote Docker smoke time. <50ms; joins `task test:arch`.
  - `task test:int:compile` — `go build -tags=integration ./...` + `go vet -tags=integration ./...`. Added to both `task ci` and `task ci:race`. Closes the local-vs-remote CI gap that let Wave 7's missing `email` import escape: `//go:build integration` files now compile-check before push. Cost: +3-5s per local `task ci`.
  - URL design rule codified: lookups by non-primary-key use query params (`?slug=`, `?email=`), not path segments (`/by-slug/{slug}`). Sub-resources use a literal segment after `{tenantId}` (`/audit/events`, not `/activity`). Stripe / GitHub / Auth0 canon. `GET /v1/tenants/by-slug/{slug}` from ADR 0044 is grandfathered for v0.2 FE-contract compatibility. **Wave 9.1c update (ADR 0052):** canonical replacement `GET /v1/tenants?slug=acme` shipped; the old path-segment route now marked `deprecated: true` in the spec (removal v0.4+).
- Wave 9 — Architecture cleanup + authz depth + auth hardening + EDA (ADRs 0051-0057):
  - **9.1a — `internal/common/breach/` → `internal/identity/{domain/passwordpolicy,adapters}/`** (ADR 0051). TDL single-module rule: domain policy moves into its owning bounded context. `breach.Checker` → `passwordpolicy.Checker` (interface) + `adapters.OfflinePasswordList` (concrete).
  - **9.1b — `internal/common/impersonation/` → `internal/identity/{domain/impersonation,adapters}/`** (ADR 0051). Same rule. `impersonation.Session` + `impersonation.Store` (domain); `adapters.ImpersonationInMemoryStore` + `adapters.ImpersonationAuditWriter{PG,Noop}` (concrete; PG impl Phase 2-ready).
  - **9.1c — Canonical slug lookup `GET /v1/tenants?slug=acme`** (ADR 0052). Stripe / Auth0 canon: query param on the listing endpoint returns `{tenants: [0..1 match]}`. Old `GET /v1/tenants/by-slug/{slug}` marked `deprecated: true` in spec (FE-contract compat through v0.3; removal v0.4+).
  - **9.1d — Role hierarchy** (ADR 0054). `parent_role_id` column on `identity.roles` + cycle/cross-tenant detection (domain guard + DB trigger). **Organizational tree only — NO permission inheritance**: each role owns its permission set explicitly. The tree is consumed by approval workflows (9.1e) + future "manager-sees-team" queries, NOT by the PermissionResolver.
  - **9.1e — Permission-elevation approval workflow** (ADR 0055). New aggregate `permissionrequest.Request` with state machine `Pending → Approved | Denied | Cancelled`. Approver = membership's current manager (uses 9.1d hierarchy). Time-bound grant lands on the Membership permission overlay with `ExpiresAt`. Per-membership-per-permission at-most-one-Pending invariant via partial unique index. 6 new HTTP routes + 4 V1 integration events.
  - **9.2a+b — `MustChangePassword` flag + account lockout** (ADR 0053). BRD canon (line 241) — admin-provisioned credentials require forced rotation. NIST 800-63B §5.2.2 — 10 failed logins in 15-min sliding window → 15-min lockout. HTTP 423 + `Retry-After: <seconds>`. Lockout check BEFORE bcrypt verify (no timing leak). LoginResponse gains `must_change_password` (omitempty); frontend redirects to change-password screen when true.
  - **9.2c — Impersonation context propagation** (ADR 0056). Wave 4.1's deferred follow-up. RFC 8693 `act` claim flows JWT → ctx → outbox columns (`act_operator_id`/`act_session_id`/`act_reason`) → forwarder → Watermill `message.Metadata` → `AuditLoggingMiddleware` → `audit_log_entry.act_*`. New `internal/identity/app/actclaim/` ctx accessor keeps the boundary clean.
  - **9.2d — Email decoupling via subscribers** (ADR 0057). Sync `emailGateway.Send(...)` in command handlers replaced with outbox-driven events + new `EmailSender` subscriber in `internal/identity/ports/subscribers/`. Two-event split per flow: AUDIT signal (no plaintext) + ACTION signal (plaintext + recipient; short-lived in outbox per security analysis). v0.2 stays Recorder-backed; production provider swap is a composition-root change.
  - **9.3 — OpenAPI as code-of-record + spec/code drift CI gates** (ADR 0050). `TestArch_RouteHasSpecOperation` (bijective drift gate) + `task ci:openapi` (Spectral lint with LeadKart-specific rules in `.spectral.yaml`) + CI matrix alignment (fixed silent skip of Wave 6/8 arch tests cloud-side); 27 missing ops + 18 schemas added to `api/openapi.yaml` to close the existing drift (Wave 5 shipped 33 ops; code grew to 60; spec was 45% behind).
  - **9.4 — Role hierarchy refactored to join-table aggregate (ADR 0058 supersedes ADR 0054).** `parent_role_id` column on `identity.roles` retired in favour of `identity.role_hierarchy_edges` + new `rolehierarchy.Edge` aggregate (Vernon IDDD ch.7 + Khorikov §11). Cross-tenant safety becomes declarative (composite FK `(tenant_id, role_id) → (tenant_id, id)` replaces the Wave 9.1d SECURITY DEFINER trigger). Single-parent invariant via partial unique index; multi-hop cycle detection via simplified SECURITY INVOKER trigger on the edges table; soft-delete preserves audit history. Wire contract STABLE — `PATCH /api/v1/roles/{roleId}/parent` keeps the same URL + body shape (optional `reason` field added); `RoleDto.parent_role_id` populated via JOIN at read time. `RoleParentChangedV1` retired in favour of paired `RoleHierarchyEdgeEstablishedV1` + `RoleHierarchyEdgeRemovedV1` integration events. Migration 20260523000007 data-lifts existing `roles.parent_role_id` links into the new table before dropping the column + the SECURITY DEFINER hotfix function.

**Active branches:**
- `main` — production; protected via PR-only merge.
- `archive/vibe-phase1-attempt` — reference-only (the original vibe-coded Phase 1 attempt; never merged, lives forever for archaeology).

**Up next (Phase 2 — Platform module, ~5-6 weeks):** marketplace + lead credits + verification calls. Platform-tenant detection + SuperAdmin seed are NOW SHIPPED in 1.5 (above) — Phase 2 builds on them. See `BRD.md` + the master plan §"Phase 2".

**Sibling .NET repo:** `d:\Development\LeadKart\` is the source-of-truth reference. Doctrine in its `.claude/rules/` is the canonical text the Go rebuild ports faithfully — when uncertain, that's the tiebreaker.

---

## What is LeadKart-Go

Go rebuild of LeadKart (.NET 10 modular monolith). Multi-tenant SaaS for Indian PCD pharma:

- LeadKart platform team sources, verifies, and sells pharma leads to pharma companies.
- Tenants (pharma companies) manage those leads through CRM, orders, inventory, dispatch.
- A **lead** is a potential buyer (medical store, chemist, distributor) — NOT a franchise partner.

8 bounded contexts: Identity, Platform, CRM, Orders, Inventory, Dispatch, Tasks, Notifications.

Reference port from the .NET implementation at `d:\Development\LeadKart\`. BRD.md + .NET aggregates are the spec; **Go canon drives shape** (no 1:1 translation).

---

## Frontend topology — SvelteKit BFF + Go API (production canon)

LeadKart targets the canonical multi-tier web architecture used by Stripe / LinkedIn / Netflix / Airbnb / Walmart / PayPal / Etsy / Slack: **Node-runtime BFF (Backend-For-Frontend) wraps a non-Node API**. Per ADR 0043:

```
Browser  ←─ HttpOnly cookies ─→  SvelteKit (adapter-node)  ←─ Bearer ─→  Go /api/v1/*
```

- **Browser** never sees raw tokens — only HttpOnly + Secure + SameSite=Lax session cookie.
- **SvelteKit server runtime** IS the BFF (no separate Go BFF binary needed): `hooks.server.ts` reads cookie, refreshes access token, attaches `Authorization: Bearer` to outbound fetches; `+page.server.ts` handles form actions + SSR data loads.
- **Go API** stays pure bearer — no cookies, no CSRF, no HTML rendering. Same surface also serves mobile apps + partner integrations identically.

**The Go API does NOT change to support the BFF.** This is the load-bearing property: the BFF is a layer above, not a refactor below. The migration is entirely frontend-repo work (~1 week) — Go side is already correctly shaped.

**Deployment shape (v0.2-Phase 5):** co-located on the same host; one front-door (Caddy / nginx / Cloudflare) terminates TLS; `/` → SvelteKit Node server, optional `/api/v1/*` allowlist for external bearer clients. Federated (separate hosts + mTLS) lands at Phase 6+ when teams scale.

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

**Before reviewing or extending code in `domain/`, `app/`, or `ports/`:** read [`docs/doctrine/tdl_canon.md`](docs/doctrine/tdl_canon.md) and run `task review:tdl`. The canon doc captures the *why* behind every decision; the arch tests block the patterns that audits kept missing (ctx-smuggled tenant, validate-tags-in-domain, Save-on-repository, business-verb repo methods, mock-generation tools, setter methods on aggregates). Both gates exist because audits-trust-the-pattern was repeatedly wrong.

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

## Testing rules (per ADR 0019 + ADR 0062)

### Test pyramid (ThreeDotsLabs canon — ADR 0062)

| Layer | Tests | How | Target per aggregate |
|---|---|---|---|
| Domain | Business rules — invariants, state machines, VO validation | Pure unit tests | Many (~10-30) |
| App / handlers | Orchestration — calls repo, emits events, error paths | Unit tests against `<aggregate>test.FakeRepository` | Many (~5-15) |
| Adapter / SQL | SQL contract — RLS fires, JSONB round-trip, 23505 translation, outbox-row write, soft-delete partial-index | Integration tests via `pgtest.RunMain` | Few (~3-6) |

### Per-aggregate fakes (mandatory)

Every domain aggregate with a `Repository` interface has a co-located
`<aggregate>test/fake_repository.go` exposing `FakeRepository`. Pattern:

```
internal/<module>/domain/<aggregate>/
├── repository.go                  # interface
├── role.go                        # aggregate
└── <aggregate>test/
    └── fake_repository.go         # in-memory FakeRepository
```

Constraints (enforced by `internal/architecture/tdl_canon_arch_test.go`):
- `var _ <aggregate>.Repository = (*FakeRepository)(nil)` compile-time gate
- NO `sync` imports (domain-subtree concurrency-free + single-test-owner pattern)
- Faithful contract: must mirror SQL adapter's `ErrXxx` translations, sort order, soft-delete filtering, partial-unique-index semantics

### Test conventions

- Table-driven via `t.Run`. `t.Parallel()` in subtests.
- Stdlib `testing` + `go-cmp` for diffs + `testify/require` for ergonomics.
- `testcontainers-go` for integration tests; build tag `//go:build integration`.
- `goleak.VerifyTestMain` (or `goleak.Find` when wrapping with `pgtest.RunMain`) in integration packages.
- Shared `pgtest.Container` per package via `pgtest.RunMain`; per-test isolation via fresh `tenant.ID` + RLS (parallel) or `sharedPG.TruncateAll(t)` (cross-tenant serial).
- Mixing `t.Parallel()` + `TruncateAll(t)` in one test is forbidden (`TestArch_TruncateAllImpliesSerial`).
- Race detector mandatory in CI (`go test -race -shuffle=on`).

### Integration tests are SQL-contract-only (ADR 0062)

`internal/<module>/adapters/*_repository_pg_test.go` MUST test:
SQLSTATE translations, RLS enforcement, JSONB round-trips, outbox-row writes, partial-index behaviors, EXPLAIN-under-RLS index selection, DB triggers.

These tests MUST NOT exist at the SQL layer (covered by `<aggregate>test.FakeRepository`):
Pure round-trips, ErrNotFound on missing ID, domain state machines, business-rule rejections that the aggregate ctor / mutator enforces.

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
