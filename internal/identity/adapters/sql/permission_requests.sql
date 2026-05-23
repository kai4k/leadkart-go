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

-- name: ListPendingPermissionRequestsByApproverPage :many
-- Approver-side queue (state=pending). Same keyset semantics as the
-- requester page. Backed by idx_permission_requests_approver_pending.
SELECT id, tenant_id, requester_membership_id, permission_constant,
       duration_days, reason, state, approver_membership_id,
       decided_at, decision_reason, granted_override_id, expires_at,
       created_at, updated_at
FROM   identity.permission_requests
WHERE  approver_membership_id = $1
  AND  state                  = 'pending'
  AND  (created_at, id) < ($2::timestamptz, $3::uuid)
ORDER  BY created_at DESC, id DESC
LIMIT  $4;
