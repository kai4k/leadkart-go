-- Membership queries — identity.tenant_memberships is RLS+FORCE.
-- Reads through this query path see only the current tenant's rows
-- unless app.is_platform=true. Cross-tenant login resolution goes
-- through GetPersonAndActiveMembershipByEmail (in persons.sql) under
-- TxScopePlatform — single-roundtrip JOIN against the partial-unique
-- index uq_memberships_person_active.

-- name: InsertMembership :exec
-- created_by_membership_id is the audit chain — who invited this
-- user. NULL for self-bootstrapped paths (RegisterTenant first
-- admin, SuperAdmin via cmd/bootstrap). Composite FK to
-- (id, tenant_id) prevents cross-tenant audit-chain spoofing per
-- migration 20260507000008.
INSERT INTO identity.tenant_memberships (
    id, person_id, tenant_id, status, joined_at, created_by_membership_id
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetMembershipByID :one
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to,
       created_by_membership_id
FROM   identity.tenant_memberships
WHERE  id = $1;

-- name: GetActiveMembershipByPersonAndTenant :one
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to,
       created_by_membership_id
FROM   identity.tenant_memberships
WHERE  person_id = $1
  AND  tenant_id = $2
  AND  status    = 'active';

-- name: ListMembershipsForPerson :many
-- Cross-tenant list of a Person's memberships. RLS filters to current
-- tenant unless platform-bypass is set; cross-tenant enumeration runs
-- under platform context (anonymise / global-suspend cascade flows).
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to,
       created_by_membership_id
FROM   identity.tenant_memberships
WHERE  person_id = $1
ORDER  BY joined_at;

-- name: ListMembershipsInCurrentTenant :many
-- Cross-membership query under tenant scope — RLS filters to the
-- current tenant via SET LOCAL app.tenant_id. Used by tenant-admin
-- "manage users" UIs.
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to,
       created_by_membership_id
FROM   identity.tenant_memberships
ORDER  BY joined_at;

-- name: ListActiveMembershipsInTenantPage :many
-- Keyset-paginated active-only listing per ADR 0038. Backed by the
-- partial composite index idx_memberships_tenant_active_joined
-- (tenant_id, joined_at DESC, id DESC) WHERE status = 'active' —
-- planner emits Index Scan, not Seq Scan + Filter, when the cursor
-- predicate uses tuple-comparison.
--
-- Cursor semantics: (sqlc.arg(before_joined_at), sqlc.arg(before_id))
-- is the previous-page boundary. First page passes the sentinel
-- (now() + 1 day, '00000000-0000-0000-0000-000000000000') so the
-- tuple comparison admits every row.
--
-- LIMIT is page_size+1 (the "peek one extra" trick from ADR 0038);
-- the caller drops the extra row when present + uses it to set
-- next_cursor.
--
-- Status filter is hard-coded to 'active' to match the partial index;
-- inactive listing path (?status=inactive) lands as a separate query
-- when frontend asks.
SELECT id, person_id, tenant_id, status, joined_at, left_at,
       designation, department, status_message, reports_to,
       created_by_membership_id
FROM   identity.tenant_memberships
WHERE  status = 'active'
  AND  (joined_at, id) < ($1::timestamptz, $2::uuid)
ORDER  BY joined_at DESC, id DESC
LIMIT  $3;

-- name: ListSuperAdminMembershipsInTenant :many
-- Returns the active Memberships in the supplied tenant that hold a
-- role flagged is_super_admin=true. Powers the platform-tenant
-- deletion guard (cmd 20260507000008): a tenant containing any
-- SuperAdmin role-holder cannot be soft-deleted via the standard
-- tenant lifecycle commands. Queryable in O(1) via the partial index
-- idx_roles_super_admin.
SELECT m.id, m.person_id, m.tenant_id, m.status, m.joined_at,
       m.left_at, m.designation, m.department, m.status_message,
       m.reports_to, m.created_by_membership_id
FROM   identity.tenant_memberships m
JOIN   identity.role_assignments  ra ON ra.membership_id = m.id
JOIN   identity.roles             r  ON r.id = ra.role_id
WHERE  m.tenant_id     = $1
  AND  m.status        = 'active'
  AND  r.is_super_admin = true
  AND  NOT r.is_deleted;

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
--
-- expires_at NULL = perpetual (default). Set to a future timestamp by
-- the approval-workflow grant path per ADR 0055 — the resolver filters
-- expired entries at resolve time. Revoked-kind rows MUST pass NULL
-- (revocations are permanent until re-granted).
INSERT INTO identity.membership_permission_overrides (
    membership_id, permission_name, kind, tenant_id, updated_at, expires_at
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: DeletePermissionOverridesByMembership :exec
DELETE FROM identity.membership_permission_overrides
WHERE  membership_id = $1;

-- name: ListPermissionOverridesByMembership :many
SELECT membership_id, permission_name, kind, tenant_id, updated_at, expires_at
FROM   identity.membership_permission_overrides
WHERE  membership_id = $1
ORDER  BY permission_name;
