# ADR 0045 — Scoped JWT impersonation: AWS STS AssumeRole pattern + RFC 8693 `act` claim

**Status:** Accepted (Wave 4 implementation shipped; Wave 4.1 audit-log enrichment shipped in Wave 9.2c per ADR 0056)
**Date:** 2026-05-22

> **Wave 9.2c update (2026-05-23):** the "Wave 4.1 follow-up" referenced throughout this ADR — populating `audit_log_entry.act_operator_id` / `act_session_id` / `act_reason` from outbox-driven subscribers — has shipped per ADR 0056. Propagation path: HTTP `authn` middleware → ctx → outbox act_* columns → forwarder → Watermill `message.Metadata` → `AuditLoggingMiddleware` → audit row. The schema columns added by migration 20260524000001 are now populated for operator-impersonation actions.

## Context

Phase 1.5 shipped impersonation as session-only:
[`POST /v1/platform/impersonation/sessions`](../../internal/identity/ports/http.go) records `{operator_id, target_tenant_id, reason, expires_at}` to Redis, but does **not** change what the operator's JWT authorizes. The operator still acts under their original `is_platform=true` token; the session is audit-log metadata only.

This is insufficient for the realistic operator workflows:

- **Support engineer reproducing a tenant-side bug** can't actually be that tenant — they're stuck as a platform-tier caller who happens to have logged an intent.
- **Multi-step workflows on behalf of a tenant** (onboard a user, configure roles, suspend an item) require remembering to attach `X-Tenant-Id` on every request — forget once, the operator's platform-scope leaks elsewhere.
- **Audit-log readability** for the tenant viewing their own audit feed shows "Platform Operator X did Y" without context — no link to a ticket / reason / time-boxed authority.
- **Compliance** (DPDP §12, SOC2 CC4.1) requires the actor-chain be machine-readable: who actually acted, on whose behalf, why, under what time-boxed authority. Session metadata alone doesn't give that — the API request itself doesn't carry it.

The canonical fix is the **AWS STS `AssumeRole` pattern**: the operator exchanges their permanent platform credentials for time-boxed, scope-downgraded credentials pinned to the target tenant. Every API call made with those credentials authoritatively acts as the target tenant, while the audit-log preserves the actor chain via the **RFC 8693 `act` claim**.

Industry evidence:

| Vendor | Mechanism | Audit claim |
|---|---|---|
| **AWS STS** `AssumeRole` | Returns time-bounded access key + secret + session token scoped to a target role | `aws:assumed-role` + `sourceIdentity` |
| **GCP IAM** `iam.serviceAccountTokenCreator` | Returns OAuth token in target service account's identity | `sub` = target SA in JWT claims |
| **Auth0 Token Vault** | Returns scoped JWT for delegated access | RFC 8693 `act` claim |
| **Microsoft Graph** "On-Behalf-Of" (OBO) flow | Returns OBO token with target user's permissions | RFC 8693 `act` claim |
| **Salesforce** "Login as User" | Creates a session as the target user; preserves operator linkage server-side | Custom session-link table |

The `act` claim ([RFC 8693 §4.1](https://datatracker.ietf.org/doc/html/rfc8693#section-4.1)) is the canonical actor-preservation mechanism for any "X acting on behalf of Y" token.

Constraints inherited from preceding ADRs:

- **ADR 0011** — golang-jwt/jwt/v5 + refresh-token families. The impersonation flow reuses the existing issuer + family-rotation primitives; no new crypto.
- **ADR 0036** — Permission model. `is_super_user` boolean short-circuits permission checks. The impersonation token MUST downgrade this to `false` (operator is acting as a tenant admin, not as themselves).
- **ADR 0039** — Per-request scope selection. `X-Tenant-Id` header for ad-hoc; impersonation token for sustained sessions. This ADR makes the "sustained" path real.
- **ADR 0042** — Cache TTL strategy. CapabilitiesTTL is keyed by `(membership_id, security_stamp)`; the impersonation token reuses the same caller's security_stamp + the target tenant context, so cache hits work naturally.
- **ADR 0044** — Enumeration safety. The impersonation endpoint stays Platform-only (`RequirePlatform` middleware); no slug-keyed lookup → no enumeration surface.

Non-goals:

- **Person-level impersonation** (operator acts as a SPECIFIC user, not as the tenant's notional admin). Considered for future ADR; the v0.2 surface is tenant-level. Reasons: simpler model, covers 90% of support workflows, avoids the "which user's exact role" disambiguation.
- **Per-permission scope reduction beyond the target tenant's CompanyOwner role.** Operators get the full CompanyOwner permission set inside the session; further reduction is a Phase 4+ "principle-of-least-privilege per action" hardening.
- **Cross-platform federation** (operator at Platform A impersonating user at Platform B). Single-platform only.

## Decision

**Impersonation session creation mints a NEW JWT — short-lived, scope-downgraded, `act`-claim-preserving — pinned to the target tenant. The operator's frontend uses this token for the session duration. The Go API treats it like ANY tenant-admin JWT; zero handler-level special-casing.**

### Token shape

```json
{
  "sub":           "01923ab-...",                  // original operator's person_id (unchanged — that's who you ARE)
  "tenant_id":     "01923cd-...",                  // target tenant's UUID
  "tenant_slug":   "acme-pharma",                  // target tenant's slug (slug-anchored authz checks)
  "membership_id": "01923ef-...",                  // synthetic — see "Membership ID resolution" below
  "is_platform":   false,                          // DOWNGRADED — operator can't escalate during session
  "is_super_user": false,                          // DOWNGRADED — operator gets target-tenant CompanyOwner perms, not platform omnipotence
  "permissions":   ["identity.users.view", ...],  // target tenant's CompanyOwner permission set
  "security_stamp": "<operator's stamp>",          // unchanged — revoking operator's stamp kills the session

  "act": {                                         // RFC 8693 §4.1
    "sub":         "01923ab-...",                  // original operator (audit chain)
    "session_id":  "imp_01923xy-...",              // links back to the impersonation session record
    "reason":      "ticket #1234 — debug missing orders"
  },

  "aud":  "impersonation",                         // distinguishes from regular JWTs (allows independent revocation)
  "iss":  "leadkart",
  "exp":  1716580800,                              // session.ExpiresAt, capped at 4h from issuance
  "nbf":  1716576000,
  "iat":  1716576000,
  "jti":  "<uuid>"
}
```

### Membership ID resolution

The handlers (especially the audit middleware) expect `membership_id` to identify a row in `identity.tenant_memberships`. The operator does NOT have a real membership in the target tenant — they're acting on its behalf, not joining it.

Two options:

| Option | Resolution |
|---|---|
| **Synthetic membership_id** (chosen) | Generate a deterministic synthetic UUID per (operator_id, target_tenant_id, session_id). NOT stored in `tenant_memberships`. Handlers that look up the membership get `ErrNotFound`; ALL such handlers must tolerate this — they should fall back to the JWT claims for non-membership-bound operations. |
| **Borrow CompanyOwner membership_id** | Reuse the target tenant's CompanyOwner membership row. Simpler. Risk: audit log shows "CompanyOwner did X" instead of "Operator acting as CompanyOwner did X" — operator identity gets washed out unless the `act` claim is explicitly preserved in every audit row. |

Chose **synthetic** because the `act` claim should be the source of truth for "who actually acted"; the membership claim should reflect the *acting identity*, not the operator's home identity. Plus the synthetic ID is opaque + uniquely traceable to the session.

The audit middleware MUST preserve both `sub` (acting identity = synthetic membership owner) AND `act.sub` (original operator) in every audit row.

### Token lifecycle

```
1. Operator calls POST /v1/platform/impersonation/sessions
   { target_tenant_id, reason, duration_minutes }
        ↓
2. Server validates (existing logic):
   - operator has is_platform=true (slug-anchored)
   - target tenant exists + isn't the platform tenant
   - target tenant has no active SuperAdmin role
     (per existing ErrPlatformTenantUndeletable guard — extended here)
   - reason ≥ 10 chars
   - duration ≤ 4 hours (capped at 240 minutes)
        ↓
3. Server creates session record (existing Redis store) AND mints
   scoped JWT pair via Issuer.Issue:
   - access_token (short TTL, 10 min — same as regular)
   - refresh_token (bound to session_id; revoking session revokes
     the family)
        ↓
4. Response: 201 CreatedImpersonationSessionResponse {
       session_id, expires_at_utc,
       access_token,         // NEW — Wave 4 addition to existing DTO
       refresh_token,        // NEW
       access_token_expires_at
   }
        ↓
5. Operator's frontend stores BOTH:
   - original platform tokens (for "Stop Impersonating" return path)
   - new scoped tokens (for impersonation session)
   Uses scoped tokens for all API calls during the session.
        ↓
6. API requests with scoped JWT:
   - JWT-bridge middleware sets app.tenant_id = target tenant's ID
   - is_platform=false → no operator bypass; operator must act
     within target tenant's RLS scope
   - Audit middleware reads sub + act.sub + act.session_id, emits
     row: {actor=operator, on_behalf_of=target_tenant, session_id, action, ...}
        ↓
7. Session end (operator-initiated):
   DELETE /v1/platform/impersonation/sessions/{session_id}
   - Existing: deletes session record
   - NEW: revokes refresh-token family bound to session_id
     → operator's scoped access_token still works until exp (≤10min),
       but cannot refresh. Frontend falls back to operator's platform
       token after refresh fails.
        ↓
8. Session end (timeout):
   - JWT exp passes; refresh fails (family.expires_at = session
     end time); same fallback as operator-initiated end.
```

### Closed-set audience

`aud: "impersonation"` is a deliberate closed-set value distinguishing impersonation tokens from regular tokens. Lets us:

- Revoke ALL impersonation tokens cluster-wide via a single `aud` allowlist toggle (security incident response)
- Refuse impersonation tokens at endpoints that shouldn't accept them (e.g. `POST /v1/platform/impersonation/sessions` — you can't impersonate-from-an-impersonation)
- Log + alert on any non-impersonation handler that processes an impersonation token (defense-in-depth)

The verifier middleware MUST add `aud` enforcement to its checks.

### Audit log shape

Every audit-log row emitted under an impersonation session carries:

| Field | Source |
|---|---|
| `actor_id` | `claims.sub` (the synthetic acting identity — pointer back to operator via `act.sub`) |
| `tenant_id` | `claims.tenant_id` (the target tenant) |
| `act_operator_id` | `claims.act.sub` (the original operator) |
| `act_session_id` | `claims.act.session_id` |
| `act_reason` | `claims.act.reason` (denormalised from session record for at-rest queryability) |
| `correlation_id` | per-request UUID (existing) |
| `occurred_at_utc` | existing |
| `action`, `duration_ms`, `succeeded`, `payload` | existing |

`audit_log_entry` schema gains three nullable columns: `act_operator_id`, `act_session_id`, `act_reason`. Migration is additive — non-impersonation rows leave them NULL.

### Implementation phasing

This ADR seals the design. Code lands in **Wave 4 PR** (separate, ~3 days work):

1. **Migration 20260524000001**: ADD COLUMN `act_operator_id`, `act_session_id`, `act_reason` to `buildingblocks.audit_log_entry`. Nullable; existing rows unaffected.
2. **`jwt.Claims`**: add `Act *ActClaim` field; `ActClaim` carries Sub + SessionID + Reason.
3. **`jwt.Issuer.Issue`**: accept `IssueArgs.Act` + `IssueArgs.Audience`; mint with the act claim + audience.
4. **`authn.Verifier`**: enforce `aud: "impersonation"` allowlist when the route expects it; reject impersonation tokens at routes that don't.
5. **`command.CreateImpersonationSessionHandler`**: extend to mint the scoped JWT pair via Issuer; return tokens in the response.
6. **`command.EndImpersonationSessionHandler`**: revoke the refresh-token family bound to session_id.
7. **`AuditLoggingMiddleware`**: extract `Act` from claims; emit the three new columns when present.
8. **Integration tests**:
   - Operator opens session → gets scoped JWT → reads target tenant as the tenant would
   - Scoped JWT can't access `/v1/platform/*` (is_platform=false)
   - Scoped JWT can't open a sub-impersonation (aud rejection)
   - Session DELETE revokes refresh → next refresh fails
   - JWT expiry within session bound matches session.expires_at
   - Audit-log row carries operator + tenant + session_id (act-chain test)

## Consequences

**Positive:**

- **Real "act as" capability.** Support workflows, audit-readability, compliance — all unlocked.
- **Industry-canon shape.** AWS STS / Microsoft OBO / Auth0 Vault all use this exact pattern; auditors recognise it instantly.
- **Audit-log auto-captures actor chain.** No per-handler "remember to log who really acted" discipline — the `act` claim is structural.
- **Defense-in-depth via downgraded scope.** Operator running `DELETE /v1/users/{id}` during impersonation can ONLY delete users in the target tenant — operator's platform privilege doesn't leak.
- **Decoupled revocation.** Ending an impersonation session doesn't affect the operator's normal platform JWT — they continue platform work with original creds.
- **Zero handler refactoring.** The scoped JWT looks like ANY tenant-admin JWT to every handler. Only the JWT issuer + verifier + audit middleware change.

**Negative:**

- **Synthetic membership_id.** Handlers expecting a real membership row get ErrNotFound. Mitigation: all such handlers should already gracefully handle ErrNotFound; explicit integration tests verify each.
- **Frontend complexity.** Frontend stores TWO token pairs (operator's + scoped); needs UX for "Stop Impersonating" return. ~1 day of frontend work to wire properly.
- **`aud` claim enforcement on every endpoint.** Easy to miss adding the allowlist on a new route. Mitigation: arch test (build-time) asserts every `mux.Handle` registers with the expected aud list.
- **Three new audit-log columns.** Schema growth; non-impersonation rows have NULL — minor storage overhead.
- **No person-level impersonation.** Some support workflows want "act as user X to reproduce the bug" — explicitly out of scope. Future ADR if needed.

## Alternatives considered

1. **Keep session-only (status quo).** Rejected. Insufficient for the realistic workflows enumerated in Context. Compliance asks remain unanswerable.

2. **Mint a regular tenant-admin JWT (no `act` claim).** Rejected. Loses the actor chain; audit-log can't tell "operator acting as tenant" from "tenant admin acting normally". Defeats the compliance benefit.

3. **Custom `X-Operator-Acting-As` header.** Considered. Rejected because every endpoint would need to read + validate the header; trusts the client to set it correctly; doesn't compose with the existing RLS / scope-selection plumbing. The token-bound mechanism is uniform.

4. **STS-style separate credentials service.** AWS STS is a separate service per region. LeadKart's scale doesn't justify the separate service; in-process Issuer with audience discrimination suffices.

5. **Person-level impersonation (operator acts as user X, not as tenant T).** Considered for inclusion. Deferred to a future ADR because (a) the impl is meaningfully more complex (which Membership? which role assignments? what permission overrides?), and (b) tenant-level covers ~90% of operator workflows. Re-evaluate post-Phase-2 when CRM workflows surface specific needs.

6. **Long-lived impersonation tokens (no expiry).** Hard rejection. Time-boxed authority is non-negotiable for audit + compliance + blast-radius containment.

## Sources

- [RFC 8693 — OAuth 2.0 Token Exchange](https://datatracker.ietf.org/doc/html/rfc8693) — `act` claim canonical definition (§4.1).
- [AWS STS — AssumeRole API](https://docs.aws.amazon.com/STS/latest/APIReference/API_AssumeRole.html) — the canonical sustained-impersonation pattern; ~15 years of production use.
- [Microsoft Graph — On-Behalf-Of flow](https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-on-behalf-of-flow) — RFC 8693 implementation reference.
- [Auth0 — Token Vault delegated access](https://auth0.com/docs/secure/tokens/token-vault) — modern SaaS implementation of the pattern.
- [Salesforce — Login as User](https://help.salesforce.com/s/articleView?id=sf.users_login_as.htm) — enterprise-canon UX shape (separate from the technical token implementation).
- ADR 0011 — JWT + refresh-token families (substrate this ADR layers on).
- ADR 0036 — Permission model (is_super_user / is_platform semantics this ADR downgrades).
- ADR 0039 — Per-request scope selection (the canonical scope mechanism; this ADR ships the "sustained" path).
- ADR 0042 — Cache TTL strategy (CapabilitiesTTL keyed by stamp — impersonation tokens reuse the same key shape).
- ADR 0044 — Enumeration safety (the impersonation endpoint inherits the platform-only protection).


**Fitness function:** convention-only — not mechanically expressible. Scoped-JWT shape is enforced inside the impersonation handler's unit tests + the RFC 8693 act-claim integration test.
