# ADR 0054 — Role hierarchy (organizational tree; NO permission inheritance)

**Status:** Superseded by ADR 0058 (Wave 9.4) — hierarchy moved out of the `Role` aggregate into a dedicated `rolehierarchy.Edge` aggregate backed by a join table; `parent_role_id` column + the SECURITY DEFINER trigger are gone. The wire contract stays the same.
**Date:** 2026-05-23

## Context

ADR 0036 modelled `Role` as a flat per-tenant aggregate: a `name`, a `hierarchy_level` (numeric authority position; UI ordering only), a closed-set `permissions` JSONB, and the system-default + super-admin + soft-delete flags. Every Role grants what's in its own `permissions` array — no composition across roles.

Wave 9.1d adds a **single-parent organizational tree** to the existing flat catalogue. The tree expresses **who reports to whom** for downstream features (approval workflows per ADR 0055, "managers see their reports' activity" queries, escalation chains). **Permission resolution stays flat per-role** — assigning Junior Salesperson to a membership does NOT propagate the parent role's permissions to the membership.

### Why organizational-only (rejected: Microsoft Entra ID inheritance model)

Two semantic choices were considered:

1. **Inheritance semantic** (Microsoft Entra ID canon): child inherits parent's permissions + adds its own. Requires the seed catalog to be inverted (root = least privileged; leaves = most privileged), OR accept that CompanyOwner-at-root propagates `Meta.TenantAdmin` to every descendant. Both are semantically confusing for a B2B SaaS where roles narrow authority as you descend the org chart.

2. **Organizational semantic** (Salesforce Role Hierarchy canon — adopted): hierarchy is independent of permissions. Each role has its own permission set; the tree expresses management/reporting structure. Approval workflows + "manager-can-see-team's-activity" queries traverse the tree explicitly when they need to.

Adopting (2) keeps the permission model auditable ("Junior Salesperson can do EXACTLY these things; check the role's permissions array") AND preserves the pre-Wave-9.1d shape of the resolver (flat union of role grants + Membership permission overlay). The hierarchy primitive is added for ADR 0055 (approval workflows) to consume; the resolver is intentionally NOT touched.

## Decision

### 1. Single-parent tree, not multi-parent DAG

Each role has at most ONE `parent_role_id`. Multi-parent (DAG) inheritance was considered and rejected for v0.2:

- **Microsoft Entra ID** hierarchical roles — single-parent.
- **AWS IAM permission boundaries** (when hierarchical) — single-parent.
- **Salesforce Profile + Role Hierarchy** — single-parent role hierarchy.
- **Auth0 / Okta / Keycloak** — flat roles + composite roles (Keycloak's composite is sugar over flat composition; not a multi-parent DAG).

The DAG case (a role inheriting from two unrelated parents) demands cycle detection across N parents instead of one chain walk, makes UI rendering hard (no obvious tree visualisation), and operationally couples unrelated branches. None of the BRD use cases require it. If a future tenant truly needs DAG composition, the membership permission-overlay already covers the "X plus selected Y" case without forcing a schema-level multi-parent.

### 2. Three-layer cycle prevention

A cycle would let inheritance loop forever. Three independent gates:

1. **DB-level trigger** (`identity.role_check_hierarchy`, migration `20260523000002`) — fires `BEFORE INSERT OR UPDATE OF parent_role_id`. Walks `parent_role_id` upward from `NEW.parent_role_id`; aborts if it ever returns to `NEW.id`. ALSO refuses when the parent belongs to a different tenant. This is the strict gate — IMPOSSIBLE to bypass at the SQL layer.
2. **Domain-level guard** (`role.Role.ChangeParent(newParentID, ancestorLookup)`) — accepts a caller-supplied closure that returns the proposed parent's ancestor chain. If `r.id` appears in that chain, returns `role.ErrHierarchyCycle` immediately. Self-parent rejected without the closure call. Clean Go-side error for app handlers.
3. **App-level pre-validation** (`SetRoleParentHandler`) — passes `RoleRepository.GetAncestors(newParentID)` as the closure. Best ergonomic error message lands here before the DB trigger ever fires.

The three layers protect against three different failure modes:

| Layer | Catches |
|-------|---------|
| App handler | Honest mistake; gives operator a clear 422 error |
| Domain guard | Programmer error in an internal seed / batch tool |
| DB trigger | Privilege escalation / app-bypass / future tools |

Vladimir Khorikov "Pragmatic Clean Architecture" §11: domain-level invariant enforcement is the FIRST line; DB constraints are the LAST line. Both protect, only the trigger is FORCE-able.

### 3. Permission resolution stays flat — hierarchy does NOT compose permissions

`PermissionResolver.ResolveAuth` is unchanged. It fetches roles via the existing `RoleRepository.GetByIDs(membershipRoleIDs)` and composes the permission union over just the directly-assigned roles plus the Membership permission overlay. **The hierarchy primitive (`parent_role_id`) is invisible to the resolver.**

The intent: each role owns its permission set EXPLICITLY. "Senior Salesperson" defines exactly what a senior salesperson can do; it does NOT inherit Manager's permissions even though it reports to Manager organizationally. Operators see the per-role grants in the admin UI and audit logs without having to mentally compose chains.

`RoleRepository.GetAncestors(roleID)` IS provided — but consumed by approval workflows (ADR 0055) and "manager-can-see-team-activity" queries, NOT by the resolver. Caller-explicit traversal.

### 4. Cross-tenant safety

The `parent_role_id` MUST point to another role in the SAME tenant. Enforced by:

- The `identity.role_check_hierarchy` trigger compares `NEW.tenant_id` against the parent's `tenant_id`; aborts with `role hierarchy: parent role belongs to a different tenant` (mapped to `role.ErrHierarchyCrossTenant` by the adapter).
- RLS scoping at the application layer naturally hides cross-tenant rows from the domain guard's `GetAncestors` (lookup returns empty), so the domain check never sees the candidate as a cycle — the trigger is the discriminator.

Note: we deliberately did NOT add a composite FK `(tenant_id, parent_role_id) → (tenant_id, id)`. The roles PK is single-column `id`, and adding the composite FK would require a candidate-key migration first. The trigger is sufficient for v0.2; the composite FK can land later as defense-in-depth without changing any API.

### 5. Default-catalog hierarchy — deferred

`seed.DefaultRoleCatalog` ships UNCHANGED in Wave 9.1d. All default-seeded roles have `parent_role_id IS NULL` (every role is a root). Operators set `parent_role_id` via `PATCH /api/v1/roles/{roleId}/parent` per-tenant, modelling their actual org chart.

Default-seed hierarchy is deferred because:
- It would couple the cross-tenant default catalog to a specific org-chart shape that may not match a given tenant's structure.
- With no permission inheritance (decision 3), the default tree is purely documentation — operators are better served by an empty starting state + the admin UI guiding them to set parent links explicitly.
- BRD line 241 doesn't specify default-hierarchy shape; only the role catalog itself.

Phase 2+ may ship a "guided onboarding" path that suggests a default tree based on tenant questionnaire answers (size, industry vertical). Not in scope for Wave 9.

### 6. Backwards compatibility

- Existing roles have `parent_role_id IS NULL`. The resolver treats NULL parent identically to the pre-Wave-9.1d flat case (no behaviour change for permission computation).
- The membership permission overlay (granted / revoked sets) STILL applies on top of the directly-assigned roles' permissions — unchanged semantics.
- Permission catalogue (closed-set from ADR 0036) unchanged — only how role permission SETS are composed.

### 7. HTTP surface

- `PATCH /api/v1/roles/{roleId}/parent` — body `{"parent_role_id": "<uuid>" | null}`. Null/empty/omitted clears parent (role becomes root). 422 surface for cycle (`role_hierarchy_cycle`) + cross-tenant (`role_hierarchy_cross_tenant`).
- `POST /api/v1/roles` (existing) — body gains optional `parent_role_id`. Same validation, same 422 codes.
- `RoleDto` (read shape) gains `parent_role_id` (omitempty when root).
- `GET /api/v1/auth/me/capabilities` — `permissions[]` now includes inherited entries transparently. Frontend nav/tier decisions need NO changes.

URL design: `PATCH /api/v1/roles/{roleId}/parent` follows ADR 0049 — sub-resource verb noun (`/parent`), not a `/by-parent/...` lookup shape. Same shape as the existing `/permissions` + `/permissions/grant` + `/permissions/revoke` sub-resources on the role.

### 8. Integration event

`RoleParentChangedV1` (tenant-scoped) — `role_id`, `tenant_id`, `old_parent_id`, `new_parent_id`, `occurred_at_utc`. Subscribers invalidate any cached effective-permission projections for every Membership holding this role (the inherited slice shifted).

## Consequences

**Positive:**
- Operators express org hierarchy once; permission policy follows the tree.
- Overlay still works for per-user exceptions ("Y but with X revoked").
- Triple-layer cycle gate is defense-in-depth — domain catches programmer error; trigger catches privilege escalation / bulk tooling.
- Single-query transitive fetch keeps the resolver hot path cheap.

**Negative:**
- Effective permission set is no longer "what's in the JSONB column" — operators must consult the resolved set (via `/me/capabilities` or admin UI) to know what a role actually grants.
- Default catalog now grants `Meta.TenantAdmin` transitively to every descendant (CompanyOwner's grant + inheritance). Operators wanting a role to NOT inherit it MUST revoke it via the membership overlay OR set the role's parent to NULL (root).
- The DB trigger runs on every INSERT + every parent-changing UPDATE. Cost is one-or-two extra SELECTs per write; trivial for v0.2 catalog sizes.

**Neutral:**
- Multi-parent DAG explicitly deferred; can be layered later if product demand emerges.
- SQL-CTE-based permission resolution deferred; in-memory walk is sufficient for v0.2 + ~Phase 2 catalog sizes.

## Deferred work

1. **Multi-parent DAG composition** — when product needs a role to inherit from two unrelated parents that aren't on the same ancestor chain. The membership overlay handles the v0.2 case; DAG is purely scaling.
2. **SQL-based permission resolution** — when the role catalog exceeds ~1000 per tenant. The `RoleRepository` interface already supports the swap; only the impl changes.
3. **UI hierarchy visualisation** — tree-rendering for the role management page. Out of scope for the API PR; lives in the SvelteKit frontend.
4. **Composite FK on `(tenant_id, parent_role_id) → (tenant_id, id)`** — defense-in-depth alongside the trigger. Requires a candidate-key migration on `roles(id, tenant_id)`; deferred until measured benefit shows.
5. **Subtree-bulk operations** ("change parent of subtree X to Y" without N independent updates) — not on Phase 2's roadmap.

## Sources

- Microsoft Entra ID — Hierarchical roles (single-parent inheritance pattern).
- AWS IAM — Permission boundaries (single-parent containment when hierarchical).
- Salesforce — Profile + Role Hierarchy (single-parent role tree).
- Auth0 / Okta / Keycloak — Composite-role docs (no DAG; flat composition).
- Vladimir Khorikov, "Pragmatic Clean Architecture" §11 — aggregate boundaries + invariant-enforcement layers.
- Eric Evans, "Domain-Driven Design" — aggregate invariants + repository contracts.
- ADR 0036 — Permission model (closed-set catalogue this ADR extends).
- ADR 0047 — Layer-boundary discipline (handler depends on `role.Repository` interface only).
- ADR 0049 — URL design rules (sub-resource verb-noun for `/parent`).
- BRD line 241 + sibling .NET `multi-tenancy.md` "SuperUser god-mode" (canonical text the Go rebuild ports).
