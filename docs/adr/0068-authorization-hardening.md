# ADR 0068 — Authorization hardening: tenant.admin implication, self-service rules, X-Tenant-Id scoping, must-change enforcement, refresh grace

**Status:** Accepted
**Date:** 2026-11-19
**Supersedes (in part):** [ADR 0036](0036-permission-model.md) (resolution rule), [ADR 0053](0053-must-change-password-and-account-lockout.md) (deferred work #1), [ADR 0039](0039-per-request-scope-selection.md) (no-header operator scope)
**Builds on:** [ADR 0044](0044-enumeration-safety-404-on-no-access.md), [ADR 0045](0045-scoped-jwt-impersonation-act-claim.md), [ADR 0055](0055-permission-elevation-approval-workflow.md)

## Context

A security audit of the identity module and the BFF contract found a family of authorization defects sharing one root: most mutating identity routes were gated by authentication only (`auth(...)`), so any authenticated tenant member could call them.

- **Authentication-only mutations.** Any member could assign roles, override permissions, create users and edit roles, including granting themselves `tenant.admin`. Role assignment was equivalent to `tenant.admin` because nothing bounded what a caller could confer.
- **`tenant.admin` did not mean what the ADRs say it means.** ADR 0061 amendment 1, ADR 0045 and the seed godoc treat it as full operational authority inside the tenant, but the resolution rule did not satisfy catalogue permissions from it uniformly, and its reach into other modules was undefined.
- **`X-Tenant-Id` (ADR 0039).** The header was not honoured as the ADR specifies: the ADR says it is IGNORED for everyone but slug-anchored platform operators, and malformed or unknown values need defined errors.
- **`must_change_password` (ADR 0053 deferred work #1)** was a login-response flag only; nothing enforced it on the API.
- **Impersonation tokens (ADR 0045)** were accepted on self-service routes and by `RequirePlatform`, were not checked against a live session, and the audience/`act` agreement was not enforced.
- **Refresh rotation** had no grace window, so a lost response (network drop, double tab) revoked the whole family as reuse.
- **Rate limiting and enumeration.** The limiter keyed on the raw peer address and had no per-route tightening; a 423 on login revealed that an account exists (ten probes against an unknown email never lock); password-reset response time was a timing oracle.
- **Password policy** stored per tenant was not enforced on create, change or reset.
- **Tenant registration** was anonymous although ADR 0053 states there is no public sign-up surface.

## Decision

**D1 – what `tenant.admin` grants.** `tenant.admin` satisfies every catalogue permission, including other modules' (crm, inventory, orders, tasks, dispatch, notifications, marketplace, lead-credits read), but only in the caller's own tenant (claims `tenant_id` must equal the effective tenant). It never satisfies platform-reserved permissions: `platform.tenants.*`, `platform.users.*`, `platform.roles.*`, `platform.unverified_contacts.manage`, `platform.lead_credits.topup`, `identity.tenants.{suspend,activate,delete}`, `identity.users.anonymise`. Grounds: ADR 0061 amendment 1 (CompanyOwner short-circuits via `Meta.TenantAdmin`), the seed godoc ("full operational authority"), ADR 0045 (operators get the full CompanyOwner permission set in-session; the impersonation token carries `["tenant.admin"]`). Re-seeding was rejected: ADR 0036 Alternative 5 argues against enumerating permissions on a role, and stored roles would need a data migration whenever the catalogue grows.

**D2 – platform equivalents on the unified surface.** On `/v1/users`, `/v1/roles`, `/v1/tenants` (ADR 0039 Context), a slug-anchored, non-impersonation platform operator also satisfies `identity.users|roles|tenants.*` through the `platform.*` catalogue entries (view to view|manage, mutations to manage, users.create to create|manage). This mirrors the .NET `PlatformTenant{User,Role,Management}Endpoints` gates.

**D3 – self-service rules.** `GET /users/{id}` and `PATCH /users/{id}/profile` pass for self (path id equals the caller's `membership_id`) or with `users.view` / `users.update`. Roles, permission overrides, deactivate/reactivate and manager changes are denied on self for everyone except super users, preventing both self-escalation and owner self-lockout.

**D4 – no grant beyond your own authority.** A caller may only confer permissions they themselves satisfy: role assignment checks the role's permission set, and only super users may assign `is_super_admin` roles. Role permission grant/replace and user override grants are checked the same way. Grounds: ADR 0036 closed set and ADR 0055 "privilege plane".

**D5 – permission-request routes stay as they are.** List/get/submit/approve/deny/cancel remain authentication-only with handler-inline checks, which is the design of ADR 0055 section 8 (list is scoped to the caller's own membership; get collapses to 404). They are on the arch-test allowlist.

**D6 – tenant registration is operator-only.** `POST /api/v1/tenants` is not self-service (ADR 0053: B2B, operator-onboarded, no public sign-up surface). New gate: `RequirePlatform` plus `platform.tenants.create`, with its own rate limit.

**D7 – `X-Tenant-Id` (ADR 0039 decision tree).** The header is honoured only for slug-anchored, non-impersonation `is_platform` callers; for everyone else it is ignored, with a debug log line. A malformed value yields 400 `invalid_tenant_scope`; an unknown or deleted tenant yields 404 `tenant_scope_not_found`. On `/tenants/{tenantId}` routes the path wins, and a header that differs from the path yields 400 `tenant_scope_conflict`. Audit is one structured `slog` INFO line per scoped request with ADR 0039's fields (`scope_mode`, `effective_tenant_id`, `caller_tenant_id`); ADR 0056 defers HTTP-side audit rows. Capabilities ignores the header (claims only). Minimum-faithful deviation (owner ruling): without the header, operators act in their own tenant — no TxScopePlatform reads and no `ambiguous_scope` 403. Because no-header is never cross-tenant, ADR 0039 Alternative 4's blast-radius hazard cannot occur.

**D8 – `must_change_password` enforcement.** It becomes a JWT claim, as ADR 0053 deferred work #1 specifies, with an allowlist middleware; impersonation is exempt. Login and refresh mint it from the Person row. The allowlist is `POST /auth/change-password` and `GET /auth/me/capabilities` (logout and refresh are anonymous). Everything else returns 403 `password_change_required`. The claim cannot go stale: changing the password rotates the stamp and kills the token.

**D9 – impersonation tokens (ADR 0045 closed-set audience).** Self-service identity routes (change-password, email change, sessions, `/me/activity`, permission-request mutations) return 403 `impersonation_not_allowed`. `RequirePlatform` rejects them explicitly. `aud=leadkart-impersonation` must agree with the presence of `act`, otherwise 401. The token is honoured only while its session exists (ADR 0045 step 7 bounds post-session access to 10 minutes, but tokens live up to 4 hours, so liveness is checked). Their reach comes from D1.

**D10 – refresh grace window.** A just-rotated token re-presented within `Refresh.GraceWindow` (default 30 s), while its direct successor is still the unconsumed chain head, receives the same successor again: no state change, no event. The successor is derived deterministically — HMAC-SHA256 with a key derived by HKDF from the JWT signing key, over family id and presented plaintext — so the server never stores plaintext and no schema change is needed. Outside the window, or once the successor has been used, presenting the old token is reuse and the family is revoked (RFC 9700 rotation; Auth0 reuse interval / Okta grace period canon).

**D11 – rate limiting stays in memory (ADR 0030 defers Redis).** Add a trusted-proxy client-IP keyer reading `X-Forwarded-For` only from trusted CIDRs, walking right-to-left and taking the first untrusted hop. Add configurable tighter per-route limits for login, refresh, request-password-reset, reset-password, confirm-email-change and tenant registration. The idempotency caller scope uses the same client IP.

**D12 – enumeration.** ADR 0053's argument that "the attacker already knew" is wrong: ten probes against an unknown email never lock, so a 423 reveals the account exists. Every no-account branch (unknown email, inactive, anonymised) runs a phantom lockout counter with the same thresholds, keeping ADR 0053's 423 UX and restoring ADR 0044's non-disclosure. The locked branch runs a dummy argon2 verify and still never verifies the real password. Request-password-reset has a fixed 300 ms response floor.

**D13 – password policy.** The stored tenant policy is enforced on create-user (target tenant), change-password (caller's tenant) and reset (active membership's tenant, else default). Registration uses `DefaultPasswordPolicy`. Rule codes: `min_length`, `requires_uppercase`, `requires_lowercase`, `requires_digit`, `requires_symbol` (a symbol is `unicode.IsPunct||IsSymbol`; spaces do not count). Response: 422 `password_policy_violation` with `errors{<field>:[rules]}`.

## Consequences

- Ordinary members (for example a Sales Executive) lose the tenant directory (`GET /users`, `GET /roles`) and tenant profile (`GET /tenants/{id}`) unless granted `*.view`.
- The web's operator console is the only tenant-registration caller; anonymous registration is gone.
- The default password policy requires all four character classes, contrary to NIST 800-63B composition guidance; the stored policy is enforced as is, and the owner may relax `DefaultPasswordPolicy` later.
- Permission resolution is a single decision (`Satisfies`) reused by every gate (`RequireTenantPermission`, `RequirePermissionOrSelf`, `DenySelfTarget`), so the rules above have one implementation.
- Existing roles need no data migration when the catalogue grows, because `tenant.admin` is an implication, not an enumeration (D1).

## Follow-up work (not in this ADR's scope)

- **No last-CompanyOwner protection.** The domain has no invariant preventing the last owner being deactivated by a `users.deactivate` holder. A domain invariant is follow-up work.
- **Impersonation session store is in memory**, so the D9 liveness check is only correct with a single replica (acceptable for v0.2, ADR 0030). A Redis-backed store is follow-up work.
- **Redis-backed rate limiter** (D11) is deferred with ADR 0030; limits are per replica until then.

## Fitness function

`TestArch_MutatingRoutesRequireAuthorization` (in `internal/architecture/authz_arch_test.go`) fails when any module registers a POST/PUT/PATCH/DELETE route whose handler carries authentication only. The sole escape hatch is `selfServiceMutationAllowList`, whose entries each carry a reason (self-service on the caller's own identity, or handler-inline authorization mandated by ADR 0053 / ADR 0055 section 8); an entry that no module registers fails the test, so the list cannot go stale. Route-to-spec parity remains gated by `TestArch_RouteHasSpecOperation`.
