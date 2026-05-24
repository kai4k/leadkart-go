# ADR 0058 — Role hierarchy as a join-table aggregate

**Status:** Accepted — supersedes ADR 0054 (Wave 9.1d).
**Date:** 2026-05-23

## Context

ADR 0054 (Wave 9.1d) modelled role hierarchy as a self-referential `parent_role_id` column on `identity.roles`. The shape worked for the basic case but accumulated three pieces of evidence that the model was wrong:

1. **Migration 20260523000005 (SECURITY DEFINER hotfix).** The original cycle/cross-tenant trigger ran SECURITY INVOKER. Under RLS+FORCE on `identity.roles` the trigger's internal `SELECT tenant_id FROM identity.roles WHERE id = NEW.parent_role_id` was silently filtered when the session's tenant scope differed from the parent's tenant — the trigger saw the parent as "doesn't exist" + the cross-tenant check fell through. Fix required SECURITY DEFINER. SECURITY DEFINER is a known footgun (search_path attack surface; the migration pinned it but the pattern is fragile).
2. **No audit metadata.** A `parent_role_id` change carried no `established_by`, `established_at`, `removed_by`, `removal_reason`. The `RoleParentChangedV1` integration event captured the transition but the row itself had no history; a stale link couldn't be forensically attributed.
3. **No extension surface.** Phase 2+ wishlist items (time-bound approver chains, multi-parent for matrix orgs, approval audit trails) had no clean place to land. Each would have needed a sibling table OR a denormalised JSON column on `roles`.

Per Vernon IDDD ch.7 + Khorikov "Pragmatic Clean Architecture" §11 — when a relationship has its own lifecycle, audit trail, or extension potential, it deserves its own aggregate. Storing it as a column on one side conflates two bounded concerns: the role's identity + the relationship's identity.

Wave 9.1e (ADR 0055) shipped `permissionrequest.Request` with exactly this shape: aggregate-per-relationship + soft-delete audit + at-most-one-active-per-key invariant via partial unique index. The role-hierarchy refactor lifts that template wholesale.

## Decision

### 1. Hierarchy is its own aggregate: `rolehierarchy.Edge`

`internal/identity/domain/rolehierarchy/` holds the `Edge` aggregate. Each row in the new `identity.role_hierarchy_edges` table is one directed parent→child edge. The `Role` aggregate loses its `parentRoleID` field, its `ChangeParent` method, and its `ErrHierarchyCycle` / `ErrHierarchyCrossTenant` sentinels. Hierarchy is no longer a property of Role — it's a sibling aggregate.

Edge state:

```
established_at, established_by_membership_id, reason (nullable)
removed_at, removed_by_membership_id, removal_reason (all nullable; non-zero = soft-deleted)
```

Lifecycle: `Active → Removed` (soft-delete). Removed edges stay in the table for audit — same template as `permissionrequest.Request`'s terminal-state-as-history. No "re-activate" transition; if a link needs to come back, create a new edge (separate audit row).

### 2. Cross-tenant safety becomes declarative

The single biggest carrying gain. The composite FK tuple

```sql
FOREIGN KEY (tenant_id, child_role_id)  REFERENCES identity.roles(tenant_id, id)
FOREIGN KEY (tenant_id, parent_role_id) REFERENCES identity.roles(tenant_id, id)
```

natively enforces same-tenant for both endpoints. No PL/pgSQL trigger, no SECURITY DEFINER, no search_path pinning. The composite FK fires SQLSTATE 23503 on tenant mismatch; the adapter translates to `rolehierarchy.ErrCrossTenant`.

To make `(tenant_id, id)` a valid FK target the migration adds `UNIQUE (tenant_id, id)` to `identity.roles`. The PK stays on `id` alone (unchanged).

### 3. Single-parent invariant via partial unique index

```sql
CREATE UNIQUE INDEX uq_role_hierarchy_active_edge_per_child
  ON identity.role_hierarchy_edges (tenant_id, child_role_id)
  WHERE removed_at IS NULL;
```

At most one ACTIVE edge per child per tenant. Adapter translates the 23505 to `rolehierarchy.ErrEdgeAlreadyExists`. The handler's atomic-replacement path (soft-delete-old + insert-new in one UoW tx) prevents transient violations during repointing.

### 4. Cycle detection stays in the DB, but simpler

A trigger on the edges table walks ancestors WITHIN the current tenant. Because the composite FK guarantees every edge in a chain shares the same tenant, the trigger can run **SECURITY INVOKER** (RLS-safe) — no privilege escalation needed. The trigger only catches the multi-hop case (A→B + B→A inserted sequentially); self-reference is blocked declaratively by `CHECK (child_role_id <> parent_role_id)`.

Khorikov §11 layering preserved: domain `New` rejects self-reference at construction; composite FK + cycle trigger are the strict-gate fallback for app-bypass writers (admin tooling, future bulk reseed).

### 5. Wire contract is stable

The HTTP route `PATCH /api/v1/roles/{roleId}/parent` keeps the same URL with the same `SetRoleParentRequest { parent_role_id }` body. Internally the handler now manipulates edges instead of mutating the role's column. `RoleDto.parent_role_id` stays in JSON responses — populated via lookup against `rolehierarchy.Repository.GetActiveByChild` at read time (cheap; small tenant catalogs; bounded by Postgres index lookup).

Frontend code path unchanged. The only spec-side addition is an optional `reason` field on the PATCH body (10-1024 chars per the DB CHECK).

### 6. Integration events: paired establish + remove

`RoleParentChangedV1` retires. The new aggregate emits two events through the same outbox pipeline:

- `RoleHierarchyEdgeEstablishedV1` — new active edge.
- `RoleHierarchyEdgeRemovedV1` — active edge soft-deleted.

Subscribers (cached effective-permission projection invalidation, audit log, future org-chart UI) get an asymmetric signal pair that's easier to handle than the previous "(old, new) tuple" V1.

### 7. Data lift

The migration's UP step copies every existing `roles.parent_role_id IS NOT NULL` row into `role_hierarchy_edges` with synthesised audit metadata: `established_at = now()`, `established_by_membership_id = NULL` (system migration), `reason = 'migrated from roles.parent_role_id at ADR 0058'`. Forensic queries can later identify migrated edges via that marker.

The DOWN step is lossless for the dominant case — re-populates `roles.parent_role_id` from any active edges before dropping the edges table + restoring the SECURITY DEFINER trigger from migration 0005.

## Consequences

**Positive:**
- Hierarchy is a first-class aggregate; future time-bound edges / multi-approver chains / multi-parent matrix orgs land cleanly.
- Cross-tenant safety becomes declarative (composite FK) — no PL/pgSQL, no SECURITY DEFINER, no search_path pinning.
- Audit metadata (who established, when, why) lives on each edge.
- Soft-delete preserves history — forensic queries can replay the org chart at any point in time.
- Pattern alignment with `permissionrequest.Request` (ADR 0055) — same template across the module reduces cognitive load.

**Negative:**
- Read path for `RoleDto.parent_role_id` now joins a second table (N small indexed lookups in `ListRoles`). Acceptable at typical tenant catalog sizes (≤30 roles); if a future tenant grows the catalog into the thousands, swap to a bulk LEFT JOIN in the sqlc query.
- Two integration events to subscribe to instead of one — more wiring at consumer side, but each event carries cleaner intent.

**Neutral:**
- Migration is a one-shot data lift; the DOWN re-populates the column from active edges + restores the original trigger.
- The `Role` aggregate shrinks (~25 lines deleted); the `rolehierarchy` aggregate adds ~280 lines but in its own focused package.

## Alternatives considered

1. **Adjacency list + composite FK (no aggregate)** — cheaper change (add composite FK to existing column, drop the SECURITY DEFINER trigger, keep everything else). REJECTED: loses the aggregate-modeling win + still leaves audit metadata homeless.
2. **Materialised-path table (`role_hierarchy_node`)** — store each ancestor walk as a path string. REJECTED as over-engineered for v0.2 catalog sizes; recursive CTE is fast enough.
3. **App-only enforcement (drop the DB trigger)** — RLS would still hide cross-tenant rows in the cycle walk. REJECTED: violates Khorikov §11 (DB-level invariants are the last line).

## Sources

- Vaughn Vernon, "Implementing Domain-Driven Design" ch.7 — relationships as aggregates.
- Eric Evans, "Domain-Driven Design" — aggregate invariants.
- Vladimir Khorikov, "Pragmatic Clean Architecture" §11 — invariant-enforcement layering.
- Stripe multi-tenant FK pattern — composite key for declarative tenant isolation.
- ADR 0055 — Permission-elevation approval workflow (same aggregate-pattern precedent).
- ADR 0054 — Role hierarchy (superseded).
- ADR 0047 — Layer-boundary discipline (handler depends on `rolehierarchy.Repository` interface only).
- PostgreSQL docs §38.7 — SECURITY DEFINER footguns (motivates moving away from it).


## Fitness function

`TestArch_AggregatesHaveFactoryAndUnmarshal + TestArch_RepositoriesHaveUpdateByIDFn` (in `internal/architecture/`).

`rolehierarchy.Edge` is an aggregate with the canonical New/UnmarshalFromDB pair + Repository.UpdateByID.
