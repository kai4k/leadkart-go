# ADR 0036 — Permission model: closed-set catalog + Role + per-Membership overlay

**Status:** Accepted
**Date:** 2026-05-06

## Context

Phase 1 closes Identity authorization. Every authenticated request must answer "can THIS Membership perform THIS action?". The .NET LeadKart parent uses a closed-set string catalog + Role/permission junction + per-User overlay; the Go rebuild needs an equivalent that fits the language idioms (no reflection, no DI container) without losing the audit-grade closed-set guarantee that prevents typo-driven privilege grants.

Constraints inherited from preceding ADRs:

- Domain layer is framework-free (ADR 0002) — permission types cannot import HTTP/DB/JWT.
- State-based persistence (ADR 0003) — Roles + Memberships are aggregates, permissions a JSONB column on Roles + an override projection on Memberships.
- RLS-scoped reads (ADR 0006) — permission resolution runs under tenant context; no cross-tenant permission joins.

Non-goals for v0.2:

- No Platform-tenant detection (ships with the v0.3 Platform module per CLAUDE.md roadmap).
- No MFA / step-up gating on permission usage (post-launch per `security.md`).
- No Policy Object composition (CRM-tier need; lands with the v0.4 CRM module).

## Decision

**Three-layer permission model.**

### Layer 1 — Closed-set catalog (`permission` package)

`internal/identity/domain/permission/` exports `IdentityPermissions` — a struct of nested struct-fields naming every permission the system recognises:

```go
permission.IdentityPermissions.Roles.View              // "identity.roles.view"
permission.IdentityPermissions.Tenants.Delete          // "identity.tenants.delete"
permission.IdentityPermissions.Meta.TenantAdmin        // "tenant.admin"
```

The catalog is closed at compile time. Adding a permission means adding a struct field + an `intern` table entry; misspellings are detected at build time, not at runtime.

`Permission` is a value object — comparable by name, Flyweight-interned via `permission.FromConstant("name")` (panics on miss — programmer error at wiring time, not request time) + `TryFromConstant` (Result-shaped for untrusted input). `Equal()` is pointer-equality fast path with name-equality fallback.

### Layer 2 — Role aggregate (per-tenant catalog)

`internal/identity/domain/role/` defines `Role` — a tenant-scoped aggregate carrying a set of `Permission` pointers + `IsSystemDefault` (immutable seed roles refuse rename / hierarchy / delete) + `IsSuperAdmin` (the load-bearing flag that drives the SuperUser short-circuit).

13 default roles seed at tenant onboarding via `seed.ApplyDefaultRoles` (Task 19). Only `CompanyOwner` carries permissions out of the box (`Meta.TenantAdmin`); the other 12 ship empty for product UX to populate.

Persistence: JSONB array of permission name strings. Decode rejects unknown names (closed-set canon — fail-loud beats silent privilege-loss).

### Layer 3 — Per-Membership overlay

`internal/identity/domain/membership/` defines `Membership.GrantedPermissions` and `Membership.RevokedPermissions` — auto-suppressing slices: granting a previously-revoked permission removes the revoke; vice versa. The two slices never coexist for the same permission name.

Persistence: `identity.membership_permission_overrides(membership_id, permission_name, kind)` with `kind ∈ {'granted', 'revoked'}` and a CHECK constraint. Composite PK enforces "at most one override per (membership, permission)". Replace-all semantics on UpdateByID — simpler than per-row diff tracking, idempotent under retry.

### Resolution rule

`Membership.EffectivePermissions(roles)` (Task 13) computes the canonical set:

```
union(role.Permissions for r in m.RoleAssignments)
  ∪ m.GrantedPermissions
  \ m.RevokedPermissions
```

Set semantics — order is informational only. Caller passes the loaded Roles in (no Repository reach across aggregates per Vernon ch.10).

`permissions.Resolver` (Task 21) is the application service that orchestrates the load: given a `Membership`, fetch its assigned Roles via `RoleRepository.GetByIDs`, then call `EffectivePermissions`. `ResolveAuth` returns the bundled `(permissions, isSuperUser)` pair JWT issuance needs (Task 22) — folded over the same Role load to avoid a second round-trip.

### Authorization runtime

JWT carries the resolved permissions as a multi-valued `permission` claim + `is_super_user` bool (Task 22). HTTP middleware (`internal/identity/ports/authn/`, Tasks 23-24) consumes:

- `RequirePermission(verifier, permName)` — 401 on bad token; 403 on missing permission; SuperUser short-circuits.
- `RequireAnyPermission(verifier, permNames...)` — disjunctive variant.
- `RequirePlatform(verifier)` — gates `/api/v1/platform/...` on the `is_platform` claim.

All three call `permission.FromConstant(name)` at wiring time — typos panic at startup, never at request time.

### SuperUser short-circuit

`is_super_user=true` on the JWT bypasses every permission check (RequirePermission middleware short-circuits before checking the claim). Computed once at issuance (`Resolver.ResolveAuth` walks Role assignments for any with `IsSuperAdmin=true`). Per `multi-tenancy.md` "SuperUser god-mode" — the audit log is the only constraint; runtime is a single bool test.

The `is_platform` claim and `is_super_user` claim are independent in v0.2. The Platform tenant lands in v0.3 Platform module; until then SuperAdmin role can only be hand-seeded into a regular tenant for testing, and `is_platform` stays false. The claim shape ships now so v0.3 wiring is mechanical.

## Consequences

**Positive:**

- Closed-set catalog catches typos at compile time (`IdentityPermissions.Roles.Vier` is a compile error; `"identity.roles.vier"` would silently grant nothing). Auth0 / Okta / Stripe convergent canon.
- Three layers compose — adding a permission to a Role or overriding for one Membership doesn't touch the other layers.
- JWT-issuance-time resolution + `security_stamp` rotation on grant/revoke means the runtime authorization-check path is O(1) string scan against an in-memory claim slice.
- Decoupled domain — `permission` package has zero infrastructure imports; tests run in microseconds without DB.
- SuperUser bypass is a boolean comparison — no permission-list traversal, no role lookup, no DB hit.

**Negative:**

- Permission set lives on the JWT — a permission grant/revoke takes effect on the next refresh (≤ access-token TTL = 10min per `security.md`). Mitigation: `security_stamp` rotation on grant/revoke + per-request `security_stamp` revalidation forces re-login earlier than TTL when needed.
- JWT bloat — a Membership with all permissions could carry ~30+ string entries (~1.5 KB). Acceptable; well under HTTP header practical limits.
- Adding a permission requires editing both the catalog struct AND the intern table. Mitigation: failing arch test on mismatch (built into `permission_test.go`).
- Override semantics ("revoke wins over union") may surprise developers used to additive-only models. Mitigated by the canonical reference being the `Membership.EffectivePermissions` aggregate method, which is the single source of truth.

## Alternatives considered

1. **String-only permissions (no catalog)** — rejected. Stripe / Auth0 / GitHub OAuth scopes all use closed-set string namespaces server-validated against a catalog. Allowing arbitrary strings turns typos into silent privilege loss.

2. **OPA / Cedar / Casbin policy engine** — rejected for v0.2. Operational overhead (separate runtime + policy DSL + push pipeline) doesn't pay rent until rule complexity exceeds "permission name in set". Policy Object lands in v0.4 CRM module for permission + hierarchy + scope composition.

3. **Permissions on Person, not Membership** — rejected. A Person changes tenants over time (FAANG canon — Auth0 / Okta / Microsoft Entra ID 8/8 convergent on "Membership owns per-tenant context"); permissions follow Membership, not the global identity.

4. **Permission re-resolution on every request** — rejected. JWT-claim approach is the standard (RFC 7519); per-request DB resolution would 10-100× the read load on `roles` + `membership_permission_overrides`. The 10-minute lag on grant/revoke is acceptable per Auth0 / Okta canon.

5. **No SuperUser short-circuit (just enumerate every permission in the JWT)** — rejected. Operationally: every new permission added to the catalog requires re-seeding every SuperAdmin Role. Doctrinally: per `multi-tenancy.md` the SuperAdmin role IS THE highest authority — short-circuiting at the boolean is what the canon says ("AWS root account", "Microsoft Entra ID Global Administrator", 10/10 FAANG convergent).

## Sources

- [Auth0 — Permissions in Authorization Core](https://auth0.com/docs/manage-users/access-control/configure-core-rbac/roles/create-roles) — closed-set permission scopes per organization.
- [Okta — Custom Authorization Server permissions](https://developer.okta.com/docs/concepts/api-access-management/) — namespaced permission strings, server-validated against a catalog.
- [Stripe — API Keys + Restricted Keys](https://stripe.com/docs/keys#limit-access) — permission scopes as a closed catalog.
- [Microsoft Learn — App roles](https://learn.microsoft.com/en-us/entra/identity-platform/howto-add-app-roles-in-apps) — string-typed app roles validated server-side.
- LeadKart .NET — `IdentityPermissions` static class, `IdentityPermissionResolver`, `RequirePermissionFilter` (mirror reference for Go port).
- LeadKart .NET — `multi-tenancy.md` "SuperUser god-mode" + `security.md` "Access token" + "SecurityStamp rotation triggers".
- Phase 1 implementation tasks 1-26: closed-set catalog (Tasks 1-4), Role aggregate (5-10), Membership overlay + EffectivePermissions resolver (11-14), persistence (15-18), seed (19-20), Resolver service (21), JWT wiring (22), middleware (23-24), V1 events (25), E2E proof (26).
