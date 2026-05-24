# ADR 0052 — Canonical slug lookup via `?slug=` query param

**Status:** Accepted
**Date:** 2026-05-23
**Extends:** ADR 0044 (enumeration safety), ADR 0049 (URL design rules)

## Context

ADR 0044 (Wave 3) shipped `GET /api/v1/tenants/by-slug/{slug}` with enumeration-safe 404 handler-inline authz to resolve a tenant by its human-readable slug. ADR 0049 (Wave 8) subsequently codified the URL design rule:

> **Lookups by non-primary-key use query parameters, not path segments.**
> `/by-X/{X}` shape is forbidden for new additions. The existing `/v1/tenants/by-slug/{slug}` is GRANDFATHERED for v0.2 backward-compat (frontend already consumes it).

ADR 0049 explicitly tagged this as known debt — the grandfathered route trips the spectral `no-by-x-path-lookups` lint warning every CI run as a visible reminder. Wave 9.1c is the canonical migration.

Industry references for query-param lookups:

| Vendor | Canonical shape | Notes |
|---|---|---|
| **Stripe** | `GET /v1/customers?email=foo@bar.com` | Returns `{data: [0..1 match]}` — Stripe API canon |
| **Auth0 Management API** | `GET /api/v2/users-by-email?email=...` | Similar shape; query param NOT path |
| **GitHub REST** | `GET /search/users?q=...` | Lookups are search-shaped |
| **Anthropic API** | `GET /v1/messages/batches?<filters>` | Listings + filters, never path natural-key lookup |
| **Microsoft Graph** | `GET /users?$filter=mail eq '...'` | OData but same semantic — query-shaped |

The path-segment lookup (`/by-slug/{slug}`) has three concrete problems:

1. **Pattern collision (caught by ADR 0049's gate).** A path like `GET /v1/tenants/{tenantId}/anything` can overlap with `/v1/tenants/by-slug/{slug}` under Go 1.22+ ServeMux precedence rules. Wave 7 hit this exact bug (`/tenants/{tenantId}/activity` vs `/tenants/by-slug/{slug}` panicked at registration).
2. **Enumeration shape mismatch.** 404 vs 200-empty-list — the latter is what Stripe/Auth0 use to defeat enumeration (no shape difference between "no such resource" and "you can't see it").
3. **Hard-to-extend.** Adding `?status=` or `?created_after=` filters on the same endpoint is trivial; adding a sibling `/by-X/{X}` path explodes the URL space.

## Decision

**Ship the canonical replacement on the listing endpoint; mark the path-segment lookup deprecated; keep BOTH operational for v0.2 frontend-contract compatibility.**

### New canonical endpoint

```
GET /api/v1/tenants?slug=acme
→ 200 OK
  {
    "tenants": [<TenantDto>] | []
  }
```

Returns a **list of 0 or 1 match**. Enumeration-safe per ADR 0044:
- Slug doesn't resolve → `{tenants: []}`
- Slug resolves to a tenant the caller can't access → `{tenants: []}` (same shape)
- Slug resolves to the caller's tenant OR caller is a Platform operator → `{tenants: [<TenantDto>]}`

Authz model identical to the path-segment endpoint:

| Caller | Slug matches caller's tenant | Slug matches another tenant | No match | Invalid slug |
|---|---|---|---|---|
| Tenant admin | `200 {tenants: [T]}` | `200 {tenants: []}` | `200 {tenants: []}` | 400 |
| Platform operator | `200 {tenants: [T]}` | `200 {tenants: [T]}` | `200 {tenants: []}` | 400 |
| Unauthenticated | 401 (middleware) | — | — | — |

Future filters extend the same endpoint without breaking the slug semantic:
- `GET /api/v1/tenants?slug=...` (v0.2; this PR)
- `GET /api/v1/tenants?status=active` (Phase 2)
- `GET /api/v1/tenants?created_after=...&cursor=...` (Phase 2 — full listing with pagination)

### Deprecated endpoint stays working

```
GET /api/v1/tenants/by-slug/{slug}
→ 200 OK { ...TenantDto } | 404 (enumeration-safe)
```

OpenAPI marks it `deprecated: true` with a description pointing at the canonical replacement. Spectral's `no-by-x-path-lookups` rule continues to warn on it (the grandfather still trips the lint, by design — visible reminder of pending removal).

### Removal timeline

- **v0.2** — both endpoints operational. Frontend consumes `/by-slug/{slug}`. Spec marks the old route deprecated.
- **v0.3** — frontend migrates to `?slug=` on its own cadence. Backend keeps both.
- **v0.4+** — backend removes `/by-slug/{slug}`. Spectral warning disappears. Spec drops the deprecated operation; the `no-by-x-path-lookups` rule has zero matches.

## Consequences

**Positive:**

- **ADR 0049 known-debt closed (modulo the deprecation window).** The canonical-shape replacement exists; frontend has a path forward.
- **Stripe/Auth0 parity.** Listing-with-filter semantics; identical to what frontend devs already know from public-API patterns.
- **Extensible.** Future filters land on the same endpoint without URL-space churn.
- **No path-pattern conflict.** `GET /api/v1/tenants` (collection) is structurally distinct from `GET /api/v1/tenants/{tenantId}/<anything>` (member sub-resources). No risk of the Wave 7 / ADR 0049 collision class.
- **Empty-list enumeration shape.** No 404-vs-403 distinction needed at the wire — frontend just checks `result.tenants.length`.

**Negative:**

- **Two endpoints in flight.** Both must stay correct + tested through v0.3. Negligible cost — they share the same `GetTenantBySlug` query handler underneath; only the wire shape differs.
- **The deprecated-route lint warning stays visible.** That's the design (reminder); spectral surfaces it on every PR. Once the frontend migration completes and `/by-slug/{slug}` is removed, the warning will go away.

## Alternatives considered

1. **Breaking change: remove `/by-slug/{slug}` immediately.** Rejected — frontend currently consumes the endpoint; coordinated migration is more disciplined than a wire break.

2. **Add `?slug=` AS A REDIRECT to `/by-slug/{slug}`.** Rejected — that just inverts the new-canonical/old-deprecated direction. Doesn't fix anything; frontend ends up following the redirect anyway.

3. **`GET /api/v1/tenants/lookup?slug=acme` (separate "lookup" verb endpoint).** Considered. Rejected because it diverges from the Stripe `GET /v1/customers?email=` shape — the canonical pattern is `?param=` on the LISTING endpoint, not a sibling `/lookup` path. Plus future filters (`?status=`, `?created_after=`) want to compose with `?slug=` on the same endpoint.

4. **Use `X-Slug-Lookup: acme` header instead of a query param.** Rejected — caching layers + log redaction + URL-shareable links all favour query params. Headers are for cross-cutting metadata (auth, idempotency keys, trace IDs), not request body fragments.

## Sources

- ADR 0044 — Enumeration safety (the security property this preserves)
- ADR 0046 — OpenAPI spec-first (the doc gate this updates)
- ADR 0049 — URL design rules + route arch gate (the rule this complies with)
- ADR 0050 — OpenAPI as code-of-record (the arch test that validated the new route is in the spec)
- Stripe API docs — `GET /v1/customers?email=` listing canon
- Auth0 Management API — `users-by-email?email=` (query-param canon at large vendors)
- Anthropic API docs — listing-with-filter shape
- Microsoft Graph API — query-shaped lookups via $filter
- GitHub REST API — search endpoints (query-shaped)


**Fitness function:** convention-only — not mechanically expressible. URL design rule — observed in the OpenAPI spec + per-handler routing.
