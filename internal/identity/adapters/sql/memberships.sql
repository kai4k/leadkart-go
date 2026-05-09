-- Membership queries — identity.tenant_memberships is RLS+FORCE.
-- Reads through this query path see only the current tenant's rows
-- unless app.is_platform=true. Cross-tenant login resolution goes
-- through GetPersonAndActiveMembershipByEmail (in persons.sql) under
-- TxScopePlatform — single-roundtrip JOIN against the partial-unique
-- index uq_memberships_person_active.

-- name: InsertMembership :exec
INSERT INTO identity.tenant_memberships (
    id, person_id, tenant_id, status, joined_at
) VALUES ($1, $2, $3, $4, $5);

-- name: GetMembershipByID :one
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to
FROM   identity.tenant_memberships
WHERE  id = $1;

-- name: GetActiveMembershipByPersonAndTenant :one
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to
FROM   identity.tenant_memberships
WHERE  person_id = $1
  AND  tenant_id = $2
  AND  status    = 'active';

-- name: ListMembershipsForPerson :many
-- Cross-tenant list of a Person's memberships. RLS filters to current
-- tenant unless platform-bypass is set; cross-tenant enumeration runs
-- under platform context (anonymise / global-suspend cascade flows).
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to
FROM   identity.tenant_memberships
WHERE  person_id = $1
ORDER  BY joined_at;

-- name: ListMembershipsInCurrentTenant :many
-- Cross-membership query under tenant scope — RLS filters to the
-- current tenant via SET LOCAL app.tenant_id. Used by tenant-admin
-- "manage users" UIs.
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to
FROM   identity.tenant_memberships
ORDER  BY joined_at;

-- name: UpdateMembershipStatus :exec
UPDATE identity.tenant_memberships
SET    status  = $2,
       left_at = $3
WHERE  id = $1;

-- name: UpdateMembershipProfile :exec
-- Per-tenant profile fields. designation/department/status_message default
-- to '' (NOT NULL DEFAULT ''); reports_to is NULLable (top-of-tree). Caller
-- (UpdateByID) writes the aggregate's current state — no per-field diff.
UPDATE identity.tenant_memberships
SET    designation    = $2,
       department     = $3,
       status_message = $4,
       reports_to     = $5
WHERE  id = $1;

-- name: InsertRoleAssignment :exec
-- Junction row. Caller MUST guarantee role.tenant_id == membership.tenant_id;
-- the composite FK on (membership_id, tenant_id) → tenant_memberships(id, tenant_id)
-- enforces this at the schema level.
INSERT INTO identity.role_assignments (
    membership_id, role_id, tenant_id, assigned_at
) VALUES ($1, $2, $3, $4);

-- name: DeleteRoleAssignmentsByMembership :exec
-- Full clear. UpdateByID's persist step uses this + per-role InsertRoleAssignment
-- to project the aggregate's current RoleAssignments slice (replace-all
-- semantics — simpler than per-row diff tracking, idempotent).
DELETE FROM identity.role_assignments
WHERE  membership_id = $1;

-- name: ListRoleAssignmentsByMembership :many
SELECT membership_id, role_id, tenant_id, assigned_at
FROM   identity.role_assignments
WHERE  membership_id = $1
ORDER  BY assigned_at, role_id;

-- name: InsertPermissionOverride :exec
-- Per-Membership permission overlay. kind ∈ {'granted', 'revoked'} —
-- the domain layer guarantees a permission_name appears at most once
-- per Membership (see Membership.GrantPermission / RevokePermission
-- auto-suppression).
INSERT INTO identity.membership_permission_overrides (
    membership_id, permission_name, kind, tenant_id, updated_at
) VALUES ($1, $2, $3, $4, $5);

-- name: DeletePermissionOverridesByMembership :exec
DELETE FROM identity.membership_permission_overrides
WHERE  membership_id = $1;

-- name: ListPermissionOverridesByMembership :many
SELECT membership_id, permission_name, kind, tenant_id, updated_at
FROM   identity.membership_permission_overrides
WHERE  membership_id = $1
ORDER  BY permission_name;
