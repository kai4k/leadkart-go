# ADR 0044 — Enumeration safety: 404 on no-access for guessable identifiers

**Status:** Accepted
**Date:** 2026-05-22

> **Wave 9.1c update (2026-05-23):** the `GET /v1/tenants/by-slug/{slug}` endpoint shipped by this ADR has been superseded for new clients by `GET /v1/tenants?slug=acme` per ADR 0052 (Stripe-canon query-param shape; returns `{tenants: [0..1 match]}` instead of 404). The path-segment endpoint stays operational through v0.3 for frontend-contract compatibility (grandfathered per ADR 0049). The enumeration-safety property is preserved on both surfaces — empty list vs 404 are equally non-disclosing.

## Context

Phase 1.5 shipped the `RequireTenantContext` middleware ([authn.go:292](../../internal/identity/ports/authn/authn.go)) which gates `/api/v1/tenants/{tenantId}/...` routes on JWT.tenant_id matching the path-supplied UUID. It returns **403 Forbidden** when the caller's tenant claim does not match the URL.

This is fine for UUID-path endpoints because **UUIDs are not guessable** — a 128-bit random identifier has effectively zero collision probability with brute-force enumeration. 403 vs 404 makes no practical difference when the attacker cannot construct the URL without already knowing the resource.

Wave 2 introduced — and Phase 1.5 follow-ups will continue to introduce — endpoints keyed by **human-readable secondary identifiers**:

- `GET /v1/tenants/by-slug/{slug}` (this PR)
- Future: `GET /v1/persons?email=...` (per the frontend wishlist A.1 item)
- Future: `GET /v1/users/by-handle/{handle}` (Phase 4+ when handles are added)

These identifiers are **guessable**:

- Slugs are typically the company name (`acme-pharma`, `pfizer-india`, `sun-pharma`)
- Emails follow predictable patterns (`admin@acme.com`, `support@pfizer.com`)
- Handles are user-chosen + often public

If these endpoints return **403** on access-denied (the same pattern as the UUID routes), they leak existence information to attackers. An attacker can probe `GET /v1/tenants/by-slug/pfizer-india`:

- **403 Forbidden** → confirms "tenant `pfizer-india` exists, you cannot access it"
- **404 Not Found** → indistinguishable from "tenant does not exist"

The 403 response is an **enumeration primitive** — repeated probes build a list of every tenant on the platform. Even if the attacker can't read the data, knowing the customer list is competitive intelligence + a starting point for further attacks (social engineering, credential stuffing, etc.).

This ADR codifies the rule so future contributors do not accidentally introduce 403 responses on guessable-identifier endpoints.

Constraints inherited from preceding ADRs:

- ADR 0036 — Permission model: SuperUser short-circuit. Operator bypass applies BEFORE the 404 — operators see real data.
- ADR 0039 — Per-request scope selection: `is_platform=true` JWT bypasses tenant gating. Slug-anchored.
- ADR 0043 — Frontend topology: SvelteKit BFF handles cookies; Go API stays bearer-only. The 404 surface is consumed identically by the BFF and by direct API consumers.

Non-goals:

- Changing the existing 403 behaviour on UUID-path endpoints (`/v1/tenants/{tenantId}/...`). UUIDs aren't guessable; 403 is acceptable + documented; rewriting middleware adds churn without security gain. New endpoints follow this ADR; existing routes can migrate later if a separate decision is made.
- Hiding ALL response differences (timing, header order, etc.). Side-channel-resistant response-equality is a Phase 6+ hardening (against motivated attackers running statistical analyses); this ADR addresses the gross enumeration channel (status code + body).

## Decision

**Endpoints whose identifier is guessable (low-entropy, human-readable, or pattern-derivable) MUST return 404 — with response body byte-identical to the "resource does not exist" case — when the caller lacks access. Inline handler authz; the existing `RequireTenantContext` middleware does NOT apply to these routes (it returns 403; not appropriate here).**

### The rule

Every guessable-identifier endpoint follows this handler shape:

```go
func handleGetByGuessableKey(log *slog.Logger, a app.Application) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 1. Parse + validate the key. Invalid input is a client bug → 400.
        key, err := parseKey(r.PathValue("key"))
        if err != nil {
            writeError(w, http.StatusBadRequest, ErrCodeInvalidKey, err.Error())
            return
        }

        // 2. Resolve the key. Real "not found" → 404 (real shape).
        view, err := a.Queries.GetByKey.Handle(r.Context(), GetByKeyQuery{Key: key})
        switch {
        case errors.Is(err, ErrNotFound):
            writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
            return
        case err != nil:
            log.ErrorContext(r.Context(), "get by key failed", "err", err)
            writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
            return
        }

        // 3. INLINE AUTHZ GATE. Operator bypass; otherwise tenant
        //    identity must match. ENUMERATION-SAFE 404 on mismatch.
        c, _ := authn.ClaimsFromContext(r.Context())
        operator := c.IsSuperUser || (c.IsPlatform && c.TenantSlug == authn.PlatformTenantSlug)
        if !operator && c.TenantID != view.TenantID {
            // Identical surface to step 2: same status, same error code,
            // empty message. NEVER 403 here — 403 leaks existence.
            writeError(w, http.StatusNotFound, ErrCodeNotFound, "")
            return
        }

        writeJSON(w, http.StatusOK, projectToDto(view))
    })
}
```

### The byte-equality requirement

The 404 returned from "doesn't exist" and the 404 returned from "exists but no access" MUST be identical at the byte level:

- Same HTTP status code (`404`)
- Same error-code string (`tenant_not_found`, `user_not_found`, etc.)
- Same message (empty, or identical helpful-but-non-leaking text)
- No differences in headers that an attacker can use

This is enforced by a mandatory integration test pattern (see ADR 0046 EXPLAIN-like-discipline) — every guessable-key endpoint MUST have a `TestE2E_*_ResponseShapesIdentical` test that asserts `bytes.Equal(crossTenant.body, missing.body)`.

### Why inline, not middleware

`RequireTenantContext` middleware compares `r.PathValue("tenantId")` (a UUID string) to `JWT.tenant_id` BEFORE the handler runs. For guessable-key endpoints, the comparison can't happen pre-handler because the key needs DB resolution first. Two options:

| Option | Trade-off |
|---|---|
| **Custom middleware per key type** (e.g. `RequireTenantContextBySlug`) | More code, more abstraction. Justified at ≥ 3 guessable-key endpoints. |
| **Inline handler authz** (this ADR) | 5 lines per handler, very visible to reviewers, no new abstraction. |

At the current count (1 slug endpoint), inline is the right call. If guessable-key endpoints proliferate (3+), promote to a shared middleware factory.

### Why 400 for invalid format

Invalid input (malformed slug, malformed email) is a client bug, not a security concern. The slug VO already rejects bad characters / wrong length / reserved names — those errors surface as `400 invalid_slug`. This is distinct from the 404 path because:

- 400 says "your request is malformed, retry with different input"
- 404 says "the resource you asked for doesn't exist (we won't say why)"

Returning 400 on bad input doesn't reveal anything about which slugs are valid — every malformed slug fails the same way regardless of platform state.

### Industry canon evidence

Every major SaaS platform follows the 404-on-no-access pattern for guessable-identifier endpoints:

| Platform | Behaviour |
|---|---|
| **GitHub** | `GET /repos/owner/repo` returns 404 whether the repo doesn't exist OR exists but is private and you lack access. Documented in [github.com/octokit](https://docs.github.com/en/rest/overview/api-versions). |
| **Stripe** | `GET /v1/customers/{id}` returns 404 whether the customer doesn't exist OR is on another account you don't have access to. |
| **Auth0 Management API** | `GET /api/v2/users/{id}` returns 404 cross-tenant. |
| **Twilio** | `GET /Accounts/{sid}/...` returns 404 across account boundaries. |
| **Slack** | `users.info` for cross-workspace returns `user_not_found` (their 404 equivalent). |
| **Notion** | `GET /v1/pages/{id}` returns 404 if you lack workspace access. |
| **Linear** | Cross-workspace GraphQL queries return null (their analog of 404), never an authorization error. |

The pattern is universal at scale. Returning 403 on guessable-key access denial is a recognised anti-pattern (OWASP API Security Top 10 §A05:2023 "Broken Function Level Authorization" + §A01:2023 "Broken Access Control").

## Consequences

**Positive:**

- **Tenant existence is not enumerable.** External observers cannot determine which tenants run on the platform by probing slugs.
- **Industry-canon behaviour.** Matches GitHub, Stripe, Auth0, Twilio. New engineers recognise the shape immediately.
- **Byte-equality test gate.** `TestE2E_*_ResponseShapesIdentical` per endpoint catches regressions where a future contributor accidentally adds a "helpful" message to one 404 path.
- **Operator path unaffected.** Platform operators with `is_platform=true` JWT see real data — their cross-tenant probing is authorised + audit-logged.
- **Reviewer-friendly.** Inline authz check is 5-7 lines visible in every guessable-key handler. No middleware indirection to chase.

**Negative:**

- **Inline duplication.** Every guessable-key handler has the same 5-line authz block. Acceptable at ≤ 3 endpoints; promote to a middleware factory if it grows. Tracked as future work.
- **Operator UX friction.** A real tenant admin trying to access a tenant they used to belong to (e.g. after deactivation) gets the same 404 as if the tenant never existed. Acceptable trade-off — they can ask Platform Support, who can resolve via the audit log.
- **Debugging asymmetry.** Engineers debugging "why can't I see this tenant" can't tell from the response whether it exists or they lack access. Mitigation: server-side `slog.InfoContext` logs the authz-rejection with both the slug + the caller's JWT claims (operator-only readable). Logs surface the truth; the wire does not.
- **Doesn't apply retroactively to UUID endpoints.** `/v1/tenants/{tenantId}` still returns 403 on mismatch (per `RequireTenantContext`). Documented non-goal; UUIDs aren't guessable, so the 403 leak is impractical. If a future review wants global 404 uniformity, that's a separate decision + migration.

## Alternatives considered

1. **Always return 403 on access denial (status quo for UUID paths).** Rejected for guessable-key endpoints. Enables tenant enumeration. Cited as anti-pattern in OWASP API Security Top 10.

2. **Return 401 (with the implication "you need a different identity") instead of 404.** Rejected. 401 confirms the resource exists AND the caller needs different credentials → same enumeration signal as 403, plus implies "try harder" which is worse.

3. **Custom middleware `RequireTenantContextBySlug`.** Considered. Premature abstraction at 1 endpoint. Promote if guessable-key endpoints multiply.

4. **Return 200 with empty body on access denial.** Rejected. Breaks REST semantics ("we successfully returned nothing"), confuses generic HTTP clients, and a careful attacker can still distinguish empty body from missing-resource 404 anyway.

5. **Return 404 with a generic error message ("could not retrieve resource").** Borderline; current decision is empty `message` to maximize byte-equality with the "doesn't exist" path. A generic-but-identical message would also work — the load-bearing property is that both paths emit the SAME bytes.

6. **Apply 404 retroactively to UUID endpoints.** Considered + deferred. UUIDs aren't guessable so the 403 → 404 migration is cosmetic, not security-improving. Migration cost (existing clients branching on 403) is non-zero. Defer until either: (a) a separate ADR proposes global uniformity, or (b) a real customer reports the inconsistency as confusing.

## Sources

- [OWASP API Security Top 10 — A01:2023 Broken Access Control](https://owasp.org/API-Security/editions/2023/en/0xa1-broken-object-level-authorization/) — enumeration via differential responses is a recognised anti-pattern.
- [OWASP Top 10 — A07:2021 Identification and Authentication Failures](https://owasp.org/Top10/A07_2021-Identification_and_Authentication_Failures/) — covers enumeration safety in auth flows.
- [GitHub REST API — "Why am I getting 404 for a private repo?"](https://docs.github.com/en/rest/overview/troubleshooting-the-rest-api) — explicit documentation that GitHub returns 404 cross-permission.
- [Stripe API conventions](https://stripe.com/docs/api/errors) — `404 resource_missing` regardless of cross-account or genuine missing.
- [Auth0 Management API](https://auth0.com/docs/api/management/v2/users/get-users-by-id) — 404 on cross-tenant access.
- ADR 0036 — Permission model: SuperUser short-circuit (the operator bypass condition).
- ADR 0039 — Per-request scope selection: `is_platform=true` semantics, slug-anchored.
- ADR 0040 — Search strategy: pg_trgm GIN indexes back the slug lookups; this ADR specifies the auth surface above them.
- `security.md` — "Login flow — enumeration safety" rule (existing project doctrine; this ADR extends it from auth to all guessable-key reads).
