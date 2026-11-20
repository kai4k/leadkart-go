-- Permission-elevation request queries — identity.permission_requests
-- is RLS+FORCE per ADR 0055. Reads/writes flow through RLS: only the
-- current tenant's rows are visible unless app.is_platform=true.
--
-- The at-most-one-pending-per-(membership, permission) invariant lives
-- in a partial unique index (uq_permission_requests_pending); the
-- adapter translates the SQLSTATE 23505 raised on collision into
-- permissionrequest.ErrPendingRequestExists.

-- name: InsertPermissionRequest :exec
-- Pending-state row. State / decision fields NULL at creation; only
-- populated by InsertPermissionRequestDecision once Approve/Deny/Cancel
-- fires.
INSERT INTO identity.permission_requests (
    id, tenant_id, requester_membership_id, permission_constant,
    duration_days, reason, state, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $7);

-- name: GetPermissionRequestByID :one
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  id = $1;

-- name: GetPermissionRequestByIDForUpdate :one
-- The aggregate-writer load (UpdateByID): takes the row lock so every writer of a
-- request (approve, deny, cancel) serialises on it and decides on a snapshot read
-- after the previous writer committed (hierarchy round 6, I2). FOR NO KEY UPDATE,
-- like the membership and person writer loads: it conflicts with every other
-- writer but not with the FOR KEY SHARE a foreign-key check takes.
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  id = $1
FOR NO KEY UPDATE;

-- name: UpdatePermissionRequestDecision :exec
-- Approve / Deny / Cancel sets the decision-side columns under
-- the UpdateFn pattern. State enforced at the application layer via
-- the aggregate's state machine; the SQL just writes whatever the
-- aggregate computed.
UPDATE identity.permission_requests
SET    state                  = $2,
       approver_membership_id = $3,
       decided_at             = $4,
       decision_reason        = $5,
       granted_override_id    = $6,
       expires_at             = $7,
       updated_at             = $8
WHERE  id = $1;

-- name: ListPendingPermissionRequestsForMembership :many
-- Used by the requester-side endpoint + the at-most-one-pending
-- invariant guard at handler-time pre-validation.
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  requester_membership_id = $1
  AND  state                   = 'pending'
ORDER  BY created_at DESC;

-- name: ListPermissionRequestsByRequesterPage :many
-- Requester-side keyset-paginated history. Cursor walks (created_at
-- DESC, id DESC). LIMIT $4 is page_size+1 per ADR 0038 "peek one extra".
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  requester_membership_id = $1
  AND  (created_at, id) < ($2::timestamptz, $3::uuid)
ORDER  BY created_at DESC, id DESC
LIMIT  $4;

-- name: ListPendingPermissionRequestsInTenantPage :many
-- The tenant's pending queue (state=pending), keyset-paginated (created_at
-- DESC, id DESC); the approver queue filters it through the authority policy.
-- Backed by idx_permission_requests_tenant_pending (migration 20261120000003).
-- RLS already scopes the rows to the transaction's tenant; the explicit
-- tenant_id predicate keeps the plan on the partial index.
-- $5 (nullable): only the requests of members who report to that membership
-- (the approver queue of a non-admin manager), via idx_memberships_reports_to.
SELECT pr.id, pr.tenant_id, pr.requester_membership_id, pr.permission_constant,
       pr.duration_days, pr.reason, pr.state, pr.approver_membership_id,
       pr.decided_at, pr.decision_reason, pr.granted_override_id, pr.expires_at,
       pr.created_at, pr.updated_at
FROM   identity.permission_requests pr
WHERE  pr.tenant_id = $1
  AND  pr.state     = 'pending'
  AND  (pr.created_at, pr.id) < ($2::timestamptz, $3::uuid)
  AND  ($5::uuid IS NULL OR pr.requester_membership_id IN (
         SELECT m.id FROM identity.tenant_memberships m WHERE m.reports_to = $5::uuid))
ORDER  BY pr.created_at DESC, pr.id DESC
LIMIT  $4;
