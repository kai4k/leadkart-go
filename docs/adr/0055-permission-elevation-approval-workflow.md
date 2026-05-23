# ADR 0055 — Permission-elevation approval workflow

**Status:** Accepted
**Date:** 2026-05-23

## Context

Wave 9.1d (ADR 0054) added a single-parent organizational tree on `identity.roles`. The tree expresses **who reports to whom** but deliberately does NOT compose permissions across roles — the resolver remained flat per `Membership.EffectivePermissions(roles)` per ADR 0036.

The organizational tree exists to be CONSUMED by exactly this ADR: an approval workflow that lets a Membership ask for an elevated permission, lets their direct manager approve or deny, and lands the granted permission on the requester's membership overlay as a **time-bound entry** — Just-In-Time access, AWS STS / Microsoft Entra ID PIM canon.

### Why not pure role assignment?

- "Assign the Manager role to me for a week" forces operators to clone Roles. Industry consensus (Auth0 + Okta + Microsoft Entra ID + AWS IAM) is that JIT elevation lives in a PRIVILEGE plane separate from ROLE assignment.
- The existing membership permission overlay (`granted` / `revoked` slices from ADR 0036) already models per-Membership delta-from-roles. Wave 9.1e adds an OPTIONAL `expires_at` to GRANT entries so the overlay can carry bounded grants without a structural refactor.
- The approval workflow itself is a small aggregate (state machine: Pending → Approved | Denied | Cancelled). Keeping it in `identity` (rather than a fresh bounded context) avoids cross-module messaging for a feature that is intrinsically Identity-shaped.

## Decision

### 1. New aggregate `permissionrequest.Request`

Lives at `internal/identity/domain/permissionrequest/request.go`. Tenant-scoped via `tenantID`; RLS+FORCE on `identity.permission_requests`. ADR 0036 + ADR 0006 conventions.

State machine:

```
Pending → Approved   (success; bounded grant created)
        → Denied     (rejected; decision_reason REQUIRED)
        → Cancelled  (requester withdrew)
```

`Approved` is terminal from the workflow's POV; the **actual permission grant** lives on the requester's membership overlay (`identity.membership_permission_overrides` row with `expires_at` set). The Request row stays `Approved` even after the grant elapses — it's audit history per Vernon IDDD ch. 7.

No "Expired" state. Resolver-time filtering (decision 3) is sufficient at v0.2 scale; a cron sweep is deferred to Phase 2 cleanup.

### 2. Invariants

| Invariant | Enforcement |
|---|---|
| `id`, `tenantID`, `requesterMembershipID`, `permission`, all non-zero | Aggregate `New()` |
| `durationDays` in `[1, 90]` | Aggregate `New()` + DB CHECK |
| `reason` length ≥ 10 chars, ≤ 1024 | Aggregate `New()` + DB CHECK |
| Permission is in the closed-set catalogue (`permission.IsKnown`) | Aggregate `New()` |
| Approver ≠ Requester (no self-approval) | Aggregate `Approve()` + `Deny()` + DB CHECK |
| At most one PENDING per `(requester_membership_id, permission_constant)` | Partial unique index `uq_permission_requests_pending` |
| Cross-tenant requests blocked | RLS+FORCE policies on `identity.permission_requests` |
| Decision reason REQUIRED on Deny | Aggregate `Deny()` |

The at-most-one-pending invariant is the canonical example of "let the DB enforce; let the adapter translate the SQLSTATE" per Brandur Leach's "Postgres unique indexes for distributed locks" + `multi-tenancy.md` adapter pattern.

### 3. Time-bound grants live on the membership overlay; resolver filters at request time

The `membership.MembershipPermissionOverride` shape gains an optional `ExpiresAt`:

```go
type GrantedOverride struct {
    Permission *permission.Permission
    ExpiresAt  time.Time // zero = perpetual; otherwise filtered by resolver
}
```

`Membership.EffectivePermissions(roles, now)` adds a third filter clause:

```
union(role.Permissions for r in roles)
  ∪ {g.Permission : g ∈ grantedPermissions, g.ExpiresAt.IsZero() OR now.Before(g.ExpiresAt)}
  \ revokedPermissions
```

**No cron sweep.** Resolver-time filtering is sufficient at v0.2 scale (catalog sizes ≤ ~30 permissions per Membership; cost is O(N) per Resolve call, dominated by the role-permission union not the overlay filter). A Phase 2 background job MAY periodically prune rows where `expires_at < now() - 30 days` for table hygiene; until then the rows stay as forensic record of past grants. This matches Microsoft Entra ID PIM behaviour where activated assignments stay in the directory as audit history after the activation window elapses.

### 4. Approver = the requester's current `ManagerID`; Platform operators are the fallback

Per ADR 0054 the organizational tree expresses reporting structure. The approval workflow consumes that tree:

| Requester has manager? | Approver is...                                            |
|------------------------|------------------------------------------------------------|
| Yes                    | The manager (`requester.ReportsTo()`)                      |
| No (orphan / root)     | Platform operators only (JWT `is_platform=true`)          |

`Roles.Approve` (new closed-set permission `identity.roles.approve`) is granted to managers / role-leads in their default catalog so the frontend can render "you have approval authority" affordances; the actual decision lookup compares `requester.ReportsTo() == caller.MembershipID OR caller.IsPlatform == true` at the handler boundary.

Platform-operator override exists because CompanyOwner-level Memberships have no manager — without the override they could never elevate their own permissions. AWS STS AssumeRole has the same fallback shape (root-account access keys can bypass IAM checks).

### 5. Self-approval forbidden

`approver_membership_id != requester_membership_id`. Aggregate domain invariant + DB CHECK constraint. A requester "approving" their own request defeats the entire point of an approval workflow; the aggregate refuses + the DB refuses.

### 6. Closed-set permission catalog only

Only permissions in `permission.IdentityPermissions` can be requested. The aggregate validates via `permission.IsKnown(perm.Name())` at `New()`; the HTTP layer also pre-validates via `permission.TryFromConstant(permissionName)`. Belt-and-suspenders.

### 7. Cross-tenant blocked

Request lives in the same tenant as the requester membership. The HTTP layer derives `tenantID` from the loaded requester membership (which itself was loaded under tenant-scoped RLS); the request inherits that tenant. The DB-level RLS policies on `identity.permission_requests` block any cross-tenant read/write even from a platform-scoped writer that skips the application-tier check.

### 8. HTTP surface

| Method | Path                                                       | Permission gate                                                |
|--------|------------------------------------------------------------|----------------------------------------------------------------|
| POST   | `/api/v1/permission-requests`                              | `RequireFreshStamp` (any caller can submit)                    |
| GET    | `/api/v1/permission-requests?role=requester\|approver`     | `RequireFreshStamp` (caller scoped to own membership)          |
| GET    | `/api/v1/permission-requests/{requestId}`                  | `RequireFreshStamp` + handler-inline (requester / approver / platform) |
| POST   | `/api/v1/permission-requests/{requestId}/approve`          | `RequireFreshStamp` + handler-inline (manager-or-platform)     |
| POST   | `/api/v1/permission-requests/{requestId}/deny`             | `RequireFreshStamp` + handler-inline (manager-or-platform)     |
| POST   | `/api/v1/permission-requests/{requestId}/cancel`           | `RequireFreshStamp` + handler-inline (requester only)          |

URL design follows ADR 0049 — sub-resource verbs (`/approve`, `/deny`, `/cancel`) for state transitions, NOT separate top-level paths or query-parameter actions. Same shape as `/api/v1/tenants/{id}/suspend` and `/api/v1/tenants/{id}/activate`.

Cross-caller access (e.g. trying to read or cancel someone else's request) collapses to **404** per ADR 0044 enumeration safety — never tell an attacker which arm matched.

### 9. Integration events

Four V1 events (TenantScoped — every request lives in a tenant):

- `identity.permission_request_submitted.v1`
- `identity.permission_request_approved.v1`
- `identity.permission_request_denied.v1`
- `identity.permission_request_cancelled.v1`

Outbox-pattern per ADR 0008 — written same-tx as the aggregate state mutation. Subscribers:

- **Audit log** (existing buildingblocks subscriber) — every event captured.
- **Notifications** (future, Phase 2+) — Twilio SMS or email to the approver when a request is submitted; to the requester when their request is decided. Deferred per the deferred-work list.
- **SIEM correlation** (future) — operator dashboards correlating Submit + Approve cadence to surface anomalies (sudden spike of elevation requests = compromised account signal).

### 10. Authorization-cache invalidation

On Approve, the membership overlay changes → `PermissionsUpdatedEvent` fires → SecurityStamp invalidator triggers JWT rotation per `security.md` "SecurityStamp rotation triggers". The requester's existing access token continues to work for its TTL but the next refresh picks up the bounded permission. The PermissionResolver's resolver-time expiry filter handles the eventual drop-off without any explicit revocation.

## Consequences

**Positive:**

- Operators get a clean JIT-elevation surface — no role explosion, no manual permission-grant flows.
- Audit trail is built-in: every Submit, Approve, Deny, Cancel writes an outbox event consumed by the audit-log subscriber.
- Resolver-time filtering avoids a cron + the entire class of "stale revocation" bugs.
- Time-bound grants align with industry canon (AWS STS, Microsoft Entra ID PIM, Okta JIT).
- The aggregate is small + isolated; no cross-module messaging required for v0.2.

**Negative:**

- Adds a new aggregate + 4 new V1 events + 6 new HTTP routes — non-trivial surface for what looks like "another approval flow". Justified because the alternative (ad-hoc admin manual-grant calls) loses the audit + invariant guarantees.
- The Approved row stays in the table after the grant elapses — minor storage growth. A Phase 2 cleanup job can prune; not a v0.2 concern.
- The grant on the Membership overlay is filtered at resolver time but the override ROW stays in `membership_permission_overrides` until a Membership write replaces it. Operators reading the column directly will see stale grants; they MUST consult `/me/capabilities` or the admin UI for the resolved set.

**Neutral:**

- The HTTP layer is deliberately thin — handlers inline the manager-or-platform check rather than depending on a middleware. The check requires loading the requester membership (to read `ReportsTo`), which middleware can't do without DB access; pushing it down to the handler keeps middleware pure.
- No multi-approver chains in v0.2. A single approver (the manager) is enough for the BRD's current scope; chains can layer on later when product needs them.

## Deferred work

1. **SMS-via-Twilio approval notifications** — submit / approve / deny notifications for the approver + requester. Lands as a Notifications-module subscriber on the existing V1 events.
2. **Cron-driven cleanup of expired overrides** — prune `membership_permission_overrides` rows where `expires_at < now() - 30d`. Pure hygiene; resolver-time filtering already covers correctness.
3. **Mobile push approval** — same shape as the SMS path but via FCM / APNS.
4. **Multi-approver chains** — "manager AND VP must both approve" / "any two of these three managers". Schema: separate `permission_request_approvers` join table.
5. **JIT activation latency** — sub-second propagation of new bounded grants to in-flight access tokens. Today requires a refresh round-trip; future could leverage the security_stamp rotation + cache-bust path more aggressively.
6. **Auto-revoke on Person anonymise / global-suspend** — currently a person being anonymised does NOT automatically Cancel their pending requests. Lands as a subscriber on `PersonAnonymisedV1` / `PersonGloballySuspendedV1`.
7. **Strict-typed bulk-replace with mixed expiry** — `Membership.ReplacePermissionOverlays` currently takes `[]*permission.Permission` for the granted slice (every entry becomes perpetual). A future caller wanting bulk-replace with mixed expiry should take `[]GrantedOverride` directly; v0.2 has no such caller.

## Sources

- AWS STS — AssumeRole + session policies (time-bound elevation; max 12h on STS).
- AWS IAM — permission boundaries (bounded grant pattern).
- Microsoft Entra ID Privileged Identity Management (PIM) — Just-In-Time activation, approver-required, audit trail.
- Okta — Approval Workflows + Just-In-Time access add-ons.
- Auth0 — Adaptive MFA + step-up authentication (a related JIT pattern in a different plane).
- Vladimir Khorikov, "Pragmatic Clean Architecture" §11 — aggregate boundaries + invariant enforcement.
- Eric Evans / Vaughn Vernon, "Implementing Domain-Driven Design" ch. 7 — aggregate design + state machines.
- OWASP API Security Top 10 §A01:2023 — broken object-level authorization (the bug class enumeration-safe 404 defends against).
- OWASP Authentication Cheat Sheet 2025 — bounded privilege escalation requires audit.
- DPDP §12 + SOC2 CC4.1 — audit-trail completeness for privilege grants.
- RFC 8693 (Token Exchange) — `act` claim shape (analogous audit-actor pattern; relevant for the impersonation analogue in ADR 0045).
- ADR 0036 — Permission model (the closed-set catalogue this ADR consumes).
- ADR 0044 — Enumeration safety (404 collapse rule).
- ADR 0045 — Scoped JWT impersonation (related JIT pattern in a different plane).
- ADR 0049 — URL design rules (sub-resource verbs for state transitions).
- ADR 0054 — Role hierarchy (the org tree this ADR's approver-discovery rule consumes via `Membership.ReportsTo()`).
