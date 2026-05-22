-- Role queries — identity.roles is RLS+FORCE per multi-tenancy.md.
-- Reads/writes flow through RLS: only the current tenant's rows are
-- visible unless app.is_platform=true. Seeded SuperAdmin + custom
-- per-tenant roles share the same table; isolation is enforced by RLS,
-- not by the application layer.
--
-- parent_role_id (ADR 0054) — single-parent hierarchy. NULL = root.
-- Cycle + cross-tenant prevention lives in the DB trigger
-- identity.role_check_hierarchy().

-- name: InsertRole :exec
INSERT INTO identity.roles (
    id, tenant_id, name, is_system_default, is_super_admin,
    hierarchy_level, permissions, created_at, parent_role_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetRoleByID :one
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by, parent_role_id
FROM   identity.roles
WHERE  id = $1;

-- name: GetRoleByTenantAndName :one
-- Used by DefaultRoleCatalog seeding (Task 19) to detect re-seeds and
-- by RoleRepository for "lookup-by-name" admin flows. Live rows only.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by, parent_role_id
FROM   identity.roles
WHERE  tenant_id = $1
  AND  name      = $2
  AND  NOT is_deleted;

-- name: ListRolesByTenant :many
-- Live (non-deleted) catalog for the current tenant. RLS scopes to the
-- bound tenant_id GUC; passing $1 is belt-and-suspenders for explicit
-- intent + platform-bypass paths that want a specific tenant.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by, parent_role_id
FROM   identity.roles
WHERE  tenant_id  = $1
  AND  NOT is_deleted
ORDER  BY hierarchy_level, name;

-- name: GetRolesByIDs :many
-- Bulk load for the PermissionResolver (Task 21) — given a Membership's
-- RoleAssignments, hydrate each Role's permission set in one query.
-- RLS still applies; cross-tenant lookups must run under platform.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by, parent_role_id
FROM   identity.roles
WHERE  id = ANY($1::uuid[])
  AND  NOT is_deleted;

-- name: UpdateRole :exec
-- Persists the mutable Role state — name, hierarchy_level, permissions,
-- parent_role_id — under the UpdateFn pattern (Task 17).
-- is_system_default + is_super_admin + tenant_id + created_at are
-- aggregate-immutable; soft-delete uses SoftDeleteRole below.
UPDATE identity.roles
SET    name            = $2,
       hierarchy_level = $3,
       permissions     = $4,
       parent_role_id  = $5
WHERE  id = $1;

-- name: SoftDeleteRole :exec
-- One-way soft-delete. Filtered out by every other query via
-- "AND NOT is_deleted" predicate; the partial unique index on
-- (tenant_id, name) WHERE NOT is_deleted releases the name for reuse.
UPDATE identity.roles
SET    is_deleted = true,
       deleted_at = $2,
       deleted_by = $3
WHERE  id = $1;

-- name: GetRoleAncestors :many
-- ADR 0054 — recursive CTE walking parent_role_id chain UPWARD from $1.
-- The seed row ($1) is excluded from the result; only ancestors are
-- returned. Soft-deleted ancestors are STILL included (ON DELETE SET NULL
-- on the FK + the cycle check cares about set-membership, not liveness).
WITH RECURSIVE ancestor_chain AS (
    SELECT seed.id            AS rid,
           seed.parent_role_id AS pid,
           0                  AS depth
    FROM   identity.roles seed
    WHERE  seed.id = $1
    UNION ALL
    SELECT step.id, step.parent_role_id, ac.depth + 1
    FROM   identity.roles step
    INNER JOIN ancestor_chain ac ON step.id = ac.pid
    WHERE  ac.depth < 32
)
SELECT roles.id, roles.tenant_id, roles.name,
       roles.is_system_default, roles.is_super_admin,
       roles.hierarchy_level, roles.permissions, roles.created_at,
       roles.is_deleted, roles.deleted_at, roles.deleted_by,
       roles.parent_role_id
FROM   identity.roles roles
INNER JOIN ancestor_chain ON ancestor_chain.rid = roles.id
WHERE  roles.id != $1
ORDER  BY ancestor_chain.depth;

