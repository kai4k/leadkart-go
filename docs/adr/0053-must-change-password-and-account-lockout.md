# ADR 0053 — MustChangePassword + account lockout on failed logins

**Status:** Accepted
**Date:** 2026-05-23

## Context

Two auth-hardening features landed together in Wave 9.2 because they share the same domain surface (`identity.persons`), the same login-flow ordering invariants, and the same release blast radius. Both originate in the .NET LeadKart BRD (line 241 + the inherited Identity password policy) and were missing from the Go rebuild's v0.2 baseline.

### Feature A — `MustChangePassword`

Per .NET LeadKart BRD line 241: when an admin/operator creates a tenant or user, the new Person's credential carries `MustChangePassword = true`. The user is then forced through the change-password flow on first login.

Why this over email verification on registration:

- LeadKart is **B2B + operator-onboarded** — there is no public sign-up surface; admins choose initial passwords for invited users. The "click the link in your email to verify" pattern doesn't fit (the admin already knows the user; the user already received the password out-of-band).
- The threat is "admin-chosen initial password is weak / shared / phished off the welcome email" — solved by forcing rotation on first login, not by verifying email ownership.
- Auth0 + Microsoft Entra ID + Okta all expose this as a separate `force_password_reset` / `must_change_password` flag on the user record, distinct from email verification.

### Feature B — Account lockout on failed logins

Per NIST 800-63B §5.2.2 + OWASP Authentication Cheat Sheet 2025 §7.7 + OWASP API Security Top 10 §A04:2023 (Unrestricted Resource Consumption). The Go rebuild already enforces **per-IP** rate limiting at the HTTP edge — but per-IP fails against credential-stuffing attacks distributing across IPs while targeting a specific account. The defense is **per-account counter** — track failed-login attempts on `identity.persons` and lock the row after a threshold within a sliding window.

## Decision

### A. MustChangePassword

**Schema** (migration 20260523000001):

```sql
ALTER TABLE identity.persons
    ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
```

Defaults to `false` so self-rotated paths + bootstrap-seeded SuperAdmin stay clean.

**Set to true** by:
- `RegisterTenant` (the admin Person created during tenant onboarding) — admin chose the initial password.
- `CreateUser` (a user Person provisioned by an admin) — admin chose the initial password.

Implemented via a sibling factory `person.NewWithMustChangePassword(...)` that wraps `person.New(...)` and toggles the flag. Keeps invariant validation in one place; the only difference is the trailing toggle.

**Cleared to false** by:
- `Person.ChangePassword(newHash)` — successful authenticated self-change.
- `Person.ConfirmPasswordReset(token, newHash)` — successful reset via emailed token.
- `Person.MarkPasswordChanged()` — bookkeeping helper for the v0.3 middleware enforcement path (see Deferred Work).

**Login flow**: when `MustChangePassword=true`, login SUCCEEDS (returns the JWT + refresh tokens) but `LoginResponse.must_change_password=true` so the frontend redirects to the change-password screen.

**Strict middleware enforcement** (block all other endpoints when flag is true) is **deferred to v0.3**. Rationale: in v0.2 the frontend is the only client — a cooperative client honouring the flag is the load-bearing enforcement. Adding middleware enforcement now creates a chicken-and-egg with the change-password endpoint itself (which would need to be on an allowlist) and is non-trivial for impersonation tokens (operator's MustChangePassword flag doesn't apply when they're acting as someone else). Lands as a separate hardening ADR after v0.3 ships.

### B. Account lockout

**Schema** (migration 20260523000001):

```sql
ALTER TABLE identity.persons
    ADD COLUMN failed_login_count    int         NOT NULL DEFAULT 0,
    ADD COLUMN locked_until          timestamptz NULL,
    ADD COLUMN last_failed_login_at  timestamptz NULL;
```

**Thresholds** (exported constants in `internal/identity/domain/person/`):

| Constant | Value | Source |
|---|---|---|
| `MaxFailedLogins` | 10 | NIST 800-63B §5.2.2 + Auth0/Okta default |
| `LockoutWindow` | 15 min | NIST 800-63B §5.2.2 + Auth0/Okta default |
| `LockoutDuration` | 15 min | NIST 800-63B §5.2.2 + Auth0/Okta default |

Constants are exported so a future operator-tunable `PasswordPolicy` override can read them as upper bounds.

**Sliding-window semantics**: `Person.RegisterFailedLogin(now)` checks `now - lastFailedLoginAt > LockoutWindow` and resets the counter to 1 instead of incrementing. The "consecutive" requirement is sliding, not absolute. Auth0/Okta canon (10 attempts in any 15-minute window — anything older is forgiven).

**Login flow ordering** (per `internal/identity/app/command/login.go::resolveAndVerify`):

1. Resolve Person + Active Membership via `AuthRouter`.
2. **If `Person.IsLocked(now)` → return `ErrAccountLocked`** with the existing `LockedUntil` timestamp, BEFORE bcrypt verify.
3. If `!IsActive || IsAnonymised` → dummy-bcrypt + `ErrInvalidCredentials`. (Terminal admin-state — do NOT increment the failed-login counter.)
4. If Active Membership missing → real bcrypt + `ErrInvalidCredentials` (timing-flatten + count attempt).
5. Verify password. On mismatch: `RegisterFailedLogin(now)` + persist + re-check `IsLocked` (so the threshold-crossing attempt surfaces 423, not 401).
6. On match: `RegisterSuccessfulLogin()` + persist (clears counter + lockout).
7. Continue with refresh-family creation + JWT mint.

**Lockout check BEFORE bcrypt verify** is the critical ordering rule. Bcrypt takes ~50-200ms; gating after would let an attacker distinguish locked-vs-unlocked accounts by timing — the same leak we already defeat for unknown-email via `dummyHash`.

**HTTP shape** (per RFC 4918 §11.3 + RFC 7231 §7.1.3):

```
HTTP/1.1 423 Locked
Retry-After: 900
Content-Type: application/json

{
  "type":   "https://leadkart.api/errors/account_locked",
  "title":  "Locked",
  "status": 423,
  "error":  "account_locked"
}
```

`423 Locked` is the canonical HTTP code for "this resource is locked" per RFC 4918 (WebDAV) — adopted by Auth0, GitHub, GitLab, and Microsoft Identity for account-lockout scenarios. `Retry-After` uses the integer-seconds form (delta-seconds per RFC 7231 §7.1.3) since SPAs find it easier to consume than the HTTP-date form.

**Enumeration safety**: surfacing `423 account_locked` IS distinct from `401 invalid_credentials`. Per OWASP Authentication Cheat Sheet 2025 §7.7, that's the correct UX surface — the user needs "wait + retry", not "try a different password". Account-existence enumeration is already defeated upstream — unknown-email and wrong-password both collapse to `401 invalid_credentials`, so revealing "you're locked out" leaks no new bit of information about which email is registered. A locked account proves the email exists ONLY to a caller who already provided 10 valid login attempts against it; the attacker already knew.

**Persistence**: a dedicated hot-path query `UpdatePersonLockoutState` writes only the four lockout columns + drains aggregate events to the outbox. Cheaper than `UpdatePerson`'s full-aggregate UPDATE on every failed login (high frequency under brute-force). Tries to join a parent UoW transaction via `pg.TxFromContext` when one is in flight.

**Best-effort persistence**: counter persistence errors are logged-and-swallowed by the login handler. A transient DB hiccup MUST NOT prevent the caller from returning `ErrInvalidCredentials` — that creates a DoS vector (attacker flaps DB → legitimate users see 500s). Worst case: one missed increment; the next attempt re-increments. Brute-force protection is statistical, not transactional.

**Integration events** (Platform-scoped, account is global):

- `identity.person_account_locked.v1` — fires when the threshold is crossed. SIEM subscribers correlate with calling IP / device label to spot enumeration sweeps; Notifications MAY send a "your account was locked" email (Auth0/Okta default).
- `identity.person_account_unlocked.v1` — fires on successful login after the account was previously locked OR had positive failure count. Closes the SIEM correlation window.

## Consequences

**Positive**:

- BRD line 241 is now enforceable — operator-provisioned credentials surface their forced-rotation requirement to the frontend on first login.
- Per-account brute-force protection complements the existing per-IP rate limit. Credential stuffing distributed across IPs is now bounded statistically.
- 423 + `Retry-After` gives the frontend a precise UX signal — countdown timer + auto-retry instead of generic "try again".
- Schema-level columns + a dedicated query keep the hot-path cheap.

**Negative**:

- Adds 4 columns to `identity.persons` — minor row growth, partial indexes already discounting NULLs.
- Lockout decision uses wall-clock `now` from the login handler — clock drift across replicas could mean a Person locked on replica A is briefly not-locked on replica B until the row is read. The window is sub-second; accepted.
- Sliding-window arithmetic means the counter can never exceed `MaxFailedLogins` even theoretically (each call clamps to 1 on window-reset, then increments). Operators reading the column directly should know "10 = currently locked" not "10 = total lifetime failures".

**Neutral**:

- MustChangePassword middleware enforcement is deferred — v0.2 relies on the cooperative frontend. Sound trade-off for a B2B product where the only client is our own SPA.

## Deferred work

1. **Strict middleware enforcement of MustChangePassword** — block all routes except `POST /v1/auth/change-password` + `POST /v1/auth/logout` when the JWT-resident flag is true. Requires a `must_change_password` JWT claim (currently surfaced only in the login response), an allowlist middleware, and impersonation-aware behaviour (operator's flag doesn't apply when impersonating).
2. **Admin-reset force flag** — let an admin set `MustChangePassword=true` on any user as part of "reset their password" operator UX. Currently the flag flows only from creation paths.
3. **Per-tenant lockout-policy override** — `tenant.PasswordPolicy.MaxFailedAttempts` + `LockoutMinutes` columns already exist (added in migration 20260507000005 for the .NET parity surface). Wire the aggregate to read these instead of the package constants when a tenant override is set; the constants become defaults + upper bounds.
4. **Lockout-attempt event correlation** — wire `Wolverine`-style middleware to record the IP / device label / user agent on `PersonAccountLockedV1` so SIEM dashboards can see who tripped the lockout, not just that one happened.
5. **Per-account audit-log column for lockout events** — the outbox-doubles-as-audit pattern (ADR 0027) already captures this via the V1 event; consider denormalising into a per-Person counter view for operator dashboards if frequency warrants it.

## Sources

- BRD line 241 (`D:\Development\LeadKart\BRD.md`) — MustChangePassword canonical text.
- NIST Special Publication 800-63B §5.2.2 — Memorized Secret Verifiers, throttling guidance.
- OWASP Authentication Cheat Sheet 2025 — §7.4 brute-force prevention; §7.7 account-lockout UX surface.
- OWASP API Security Top 10 §A04:2023 — Unrestricted Resource Consumption.
- RFC 4918 §11.3 — 423 Locked status code.
- RFC 7231 §7.1.3 — Retry-After header semantics.
- RFC 9457 — Problem Details for HTTP APIs (response shape).
- Auth0 anti-brute-force docs — 10/15min/15min defaults + per-account lock model.
- Okta password-policy lockout — same defaults as Auth0.
- Stripe API error model — distinct 423 surface for account-suspended states.


**Fitness function:** convention-only — not mechanically expressible. MustChangePassword flag + NIST 800-63B §5.2.2 lockout policy are covered by the lockout unit tests + login integration tests.
