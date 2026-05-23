-- Role queries — identity.roles is RLS+FORCE per multi-tenancy.md.
-- Reads/writes flow through RLS: only the current tenant's rows are
-- visible unless app.is_platform=true. Seeded SuperAdmin + custom
-- per-tenant roles share the same table; isolation is enforced by RLS,
-- not by the application layer.
--
-- Hierarchy moved OUT of this table per ADR 0058 (Wave 9.4) into
-- identity.role_hierarchy_edges (its own aggregate). The
-- parent_role_id column + its DB trigger + its recursive ancestor CTE
-- all live in role_hierarchy_edges.sql / the rolehierarchy package now.

-- name: InsertRole :exec
INSERT INTO identity.roles (
    id, tenant_id, name, is_system_default, is_super_admin,
    hierarchy_level, permissions, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetRoleByID :one
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by
FROM   identity.roles
WHERE  id = $1;

-- name: GetRoleByTenantAndName :one
-- Used by DefaultRoleCatalog seeding (Task 19) to detect re-seeds and
-- by RoleRepository for "lookup-by-name" admin flows. Live rows only.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       is_deleted, deleted_at, deleted_by
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
       is_deleted, deleted_at, deleted_by
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
       is_deleted, deleted_at, deleted_by
FROM   identity.roles
WHERE  id = ANY($1::uuid[])
  AND  NOT is_deleted;

-- name: UpdateRole :exec
-- Persists the mutable Role state — name, hierarchy_level, permissions
-- — under the UpdateFn pattern (Task 17). is_system_default +
-- is_super_admin + tenant_id + created_at are aggregate-immutable;
-- soft-delete uses SoftDeleteRole below.
UPDATE identity.roles
SET    name            = $2,
       hierarchy_level = $3,
       permissions     = $4
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
