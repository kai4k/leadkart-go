# ADR 0039 — Per-request scope selection: JWT.is_platform + X-Tenant-Id header decision tree

**Status:** Accepted
**Date:** 2026-05-18

## Context

Phase 1.5 shipped two scope mechanisms ([internal/common/pg/transactor.go](../../internal/common/pg/transactor.go)):

- **`TxScopeTenant`** — sets `app.tenant_id` GUC; RLS policies admit rows where `tenant_id = app.current_tenant()`. The default for regular tenant-admin / CompanyOwner requests.
- **`TxScopePlatform`** — sets `app.is_platform = true` GUC; RLS policies have `OR app.is_platform()` bypass. Used by `PlatformStatsHandler` + the outbox forwarder for cross-tenant queries.

Routing today is **implicit by path**: handlers under `/v1/platform/*` reach for `TxScopePlatform`; everything else uses `TxScopeTenant`. Path-shape and scope-decision are conflated.

The frontend backend wishlist (committed at `fbca944`) consolidates the operator + tenant-admin surfaces into one unified path hierarchy. Operators with `is_platform=true` JWTs use `/v1/users`, `/v1/roles`, `/v1/tenants` directly (same paths as regular tenant admins), with a header-driven override identifying the target tenant. The implicit "path = scope" mapping breaks under this model; scope becomes a **per-request decision** based on (JWT claims × headers × HTTP verb).

Constraints inherited from preceding ADRs:

- ADR 0001 — modular monolith; ADR 0006 — RLS + SET LOCAL. RLS bypass cannot be "the default" — must be opt-in per request.
- ADR 0036 — closed-set permission catalog. Operators authenticated as `is_platform=true` carry SuperAdmin role; the boolean is a hard claim, not derived per-request.
- Phase 1.5 ADR `RequirePlatform` slug-anchored — JWT `is_platform=true` is verified against `tenant_slug == "platform"` in the JWT itself before any handler runs.

Non-goals:

- Cross-cluster federation. Multi-region tenant routing is Phase 5+ ops work; this ADR scopes to single-cluster Postgres + RLS.
- Per-resource permission gating beyond the existing `RequirePermission` middleware. Scope decision is orthogonal to permission decision.

## Decision

**The JWT-bridge middleware computes the scope per-request from a 4-clause decision tree, then sets the appropriate Postgres GUC for the request's transaction(s):**

```
On each authenticated request, the JWT-bridge middleware evaluates:

┌─ JWT.is_platform == false (regular tenant admin / CompanyOwner)
│  └─ TxScopeTenant with app.tenant_id = JWT.tenant_id
│     X-Tenant-Id header → IGNORED (operator-only)
│     X-Impersonation-Session-Id → IGNORED (operator-only)
│
├─ JWT.is_platform == true AND X-Tenant-Id header present
│  └─ TxScopeTenant with app.tenant_id = X-Tenant-Id header value
│     Operator reading or writing within a specific tenant's RLS scope.
│     Audit log entry captures: operator_id + target_tenant_id + reason="header-scoped"
│
├─ JWT.is_platform == true AND no X-Tenant-Id header AND read-only verb (GET)
│  └─ TxScopePlatform — operator probing across tenants
│     Audit log entry captures: operator_id + reason="platform-scope-read"
│
└─ JWT.is_platform == true AND no X-Tenant-Id header AND mutating verb (POST/PUT/PATCH/DELETE)
   └─ HTTP 403: cross-tenant mutation requires either X-Tenant-Id header
      OR an active impersonation session (X-Impersonation-Session-Id).
      Prevents accidental "platform operator clicks button → 10K tenants affected"
      catastrophes.
```

### Header conventions

| Header | Set by | Read by | Effect |
|---|---|---|---|
| `Authorization: Bearer <JWT>` | Client | JWT verifier | Identity + `is_platform` claim |
| `X-Tenant-Id: <uuid>` | Operator client | JWT-bridge middleware | Overrides scope to target tenant (only if `is_platform=true`) |
| `X-Impersonation-Session-Id: <session-uuid>` | Operator client | Impersonation middleware (post-launch) | Resolves to a target tenant + active reason; full audit chain |

`X-Tenant-Id` over `?tenant_id=` query param per Stripe Connect canon (`Stripe-Account` header): keeps tenant scope out of URLs (no access-log leakage, no browser history, no CDN cache-key pollution).

### Mutation-without-scope rule

When `is_platform=true` JWT hits a mutating verb without scope (no `X-Tenant-Id`, no impersonation session), the middleware returns **403 with `ErrCodeAmbiguousScope`** + a message hinting at the two valid scoping mechanisms. Reasoning:

- **Cross-tenant writes are a hard failure mode.** An operator running `PATCH /v1/users/{id}/role` without scope could mutate the same role-id across every tenant simultaneously. Refusing the mutation forces the operator to be explicit.
- **403 over 400.** The request is well-formed; it's the authorization context that's ambiguous. RFC 7235 favors 403 for "you must scope this further".
- **Impersonation is the canon path for sustained operator work in one tenant.** `X-Tenant-Id` is appropriate for one-off reads or quick mutations. The impersonation flow is appropriate when the operator will perform a multi-step workflow as the tenant — it captures the reason once and audits every request thereafter.

### Audit log shape under scope decisions

Every request flows through the existing audit-log middleware (per [internal/common/audit/](../../internal/common/audit/)). Scope decisions add structured fields:

| Field | Value |
|---|---|
| `scope_mode` | `"tenant"` (regular) / `"tenant-via-header"` (operator with X-Tenant-Id) / `"platform"` (operator cross-tenant read) / `"impersonation"` (operator via session) |
| `effective_tenant_id` | resolved tenant UUID, or `null` for platform-scope |
| `caller_tenant_id` | JWT.tenant_id (the operator's home tenant, always "platform" for SuperUsers) |

These let forensic queries answer "every cross-tenant read this operator performed in the last 24h" cleanly.

## Consequences

**Positive:**

- **One mental model.** The scope decision tree is a single page; new contributors don't have to grep for path patterns to know what RLS context their handler runs in.
- **Header-scoped reads are cheap.** No impersonation session creation; the operator's existing JWT + a header is all that's needed for "show me tenant X's user list". Matches Stripe Connect operator workflows.
- **Mutation safety by default.** Cross-tenant mutation requires explicit scoping; accidentally hitting "delete" without context returns 403 instead of nuking N tenants. Defense-in-depth.
- **Auditability.** Every scope choice lives in the audit log. Cross-tenant operator activity is queryable post-incident.
- **No path-level discrimination needed.** `/v1/platform/users` and `/v1/users` collapse into one. The unified-surface migration becomes a header + claim decision; routing stays simple.

**Negative:**

- **Header dependency.** Operator-side tooling (Postman collections, Cypress tests, CLI scripts) must learn `X-Tenant-Id`. Trivial but a one-time onboarding cost.
- **Middleware complexity.** Today's path-keyed routing has ~5 lines of decision logic. This ADR adds ~30 lines. Worth it for the safety + clarity wins; reviewers can read the decision tree at a glance.
- **No "default to last-used tenant" magic.** Each operator request must carry the tenant header (or use impersonation). Some UIs may want to inject a per-session default; that's a frontend concern, not a backend one. Backend stays explicit.
- **Impersonation middleware not yet shipped.** The decision tree references `X-Impersonation-Session-Id` but the resolution middleware is post-launch work ([internal/identity/ports/http.go:174](../../internal/identity/ports/http.go) "for v0.2 these endpoints manage the session lifecycle but the per-request header pickup is post-launch operational work"). Until then, impersonation = audit-only; cross-tenant mutation = `X-Tenant-Id` only. The decision tree's 4th clause stays partially enforced.

## Alternatives considered

1. **Path-keyed scope (`/v1/platform/*` stays a separate hierarchy).** The current shape. Rejected — see frontend backend wishlist's "unified surface" motivation. Every multi-tenant SaaS at scale (Stripe Connect, AWS, Auth0, Microsoft Graph, GitHub Enterprise) lands on identity-driven scope eventually; doing it now while the controller count is small (~15) is vastly cheaper than refactoring at Phase 3+ (~100 controllers).

2. **Query param `?tenant_id=` instead of `X-Tenant-Id` header.** The frontend backend wishlist originally proposed this. Rejected per Stripe Connect canon (`Stripe-Account` header) — query params get logged in access logs by default, appear in browser history + shared URLs, and pollute CDN cache keys. Headers don't. Trivial cost difference; cleaner semantics for "tenant is auth context, not a search filter".

3. **Always require impersonation for any cross-tenant operator action (no `X-Tenant-Id` header path).** Rejected as too friction-heavy for routine reads. The Stripe Connect platform doesn't require an impersonation session to view a connected account's customers list — just the `Stripe-Account` header. Forcing a session for every read inflates the audit log and slows operator workflows. The mutation-without-scope rule keeps the safety property without the read friction.

4. **Allow mutations under `is_platform=true` without scope (treat operator as global write authority).** Rejected outright. Single biggest cross-tenant blast-radius hazard in any SaaS. The whole reason `is_platform=true` exists is to bypass RLS for *reads*; writes must be scoped to a tenant.

5. **Encode scope in the JWT itself (mint a scoped JWT when the operator picks a tenant in the UI).** This IS what the impersonation flow does for sustained workflows. Rejected as the default scope mechanism because it requires a session-creation round-trip for every cross-tenant action; too heavy for one-off reads. Hybrid model: header for ad-hoc, scoped-JWT for sustained = canon.

## Sources

- [Stripe Connect API — Making API calls for connected accounts](https://stripe.com/docs/connect/authentication) — `Stripe-Account` header model.
- [AWS STS — AssumeRole](https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRole.html) — scoped credential model for sustained cross-account work.
- [Microsoft Graph — Transition identity context](https://learn.microsoft.com/en-us/graph/auth-on-behalf-of-flow) — same pattern, "on-behalf-of" tokens.
- ADR 0006 — Multi-tenancy via Postgres RLS + SET LOCAL (foundation).
- ADR 0011 — Auth: golang-jwt/jwt/v5 + refresh-token families (JWT claim surface).
- ADR 0036 — Permission model (closed-set catalog, SuperAdmin flag mechanics).
- Frontend backend wishlist (`fbca944` in this repo) — unified-surface spec motivating this ADR.


**Fitness function:** convention-only — not mechanically expressible. Scope-selection decision tree lives inside `tenancy.FromRequest`; behaviour covered by unit + integration tests.
