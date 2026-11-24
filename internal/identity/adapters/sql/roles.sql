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
       created_by_membership_id, is_active
FROM   identity.roles
WHERE  id = $1;

-- name: GetRoleByTenantAndName :one
-- Used by DefaultRoleCatalog seeding (Task 19) to detect re-seeds and
-- by RoleRepository for "lookup-by-name" admin flows. Any status: a role name is held by
-- inactive roles too (ADR 0073).
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       created_by_membership_id, is_active
FROM   identity.roles
WHERE  tenant_id = $1
  AND  name      = $2;

-- name: ListRolesByTenant :many
-- The whole catalog of the current tenant, active and inactive roles (the query handler
-- filters by status and counts). Nothing is deleted (ADR 0073). RLS scopes to the
-- bound tenant_id GUC; passing $1 is belt-and-suspenders for explicit
-- intent + platform-bypass paths that want a specific tenant.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       created_by_membership_id, is_active
FROM   identity.roles
WHERE  tenant_id  = $1
ORDER  BY hierarchy_level, name;

-- name: GetRolesByIDs :many
-- Bulk load for the PermissionResolver (Task 21) — given a Membership's
-- RoleAssignments, hydrate each Role's permission set in one query.
-- ACTIVE roles only: an inactive role confers nothing. RLS still applies; cross-tenant
-- lookups must run under platform.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       created_by_membership_id, is_active
FROM   identity.roles
WHERE  id = ANY($1::uuid[])
  AND  is_active;

-- name: GetRolesByIDsIncludingInactive :many
-- Authority facts (ADR 0069): the roles a membership is assigned INCLUDING
-- inactive ones. An inactive role confers nothing but must still be present
-- in the loaded set, so a partial load cannot make a member look weaker than
-- it is. Authority use only; every other reader uses GetRolesByIDs.
SELECT id, tenant_id, name, is_system_default, is_super_admin,
       hierarchy_level, permissions, created_at,
       created_by_membership_id, is_active
FROM   identity.roles
WHERE  id = ANY($1::uuid[]);

-- name: UpdateRole :exec
-- Persists the mutable Role state — name, hierarchy_level, permissions, is_active
-- — under the UpdateFn pattern (Task 17). is_system_default +
-- is_super_admin + tenant_id + created_at are aggregate-immutable.
UPDATE identity.roles
SET    name            = $2,
       hierarchy_level = $3,
       permissions     = $4,
       is_active       = $5
WHERE  id = $1;
