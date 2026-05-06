-- Membership queries — identity.tenant_memberships is RLS+FORCE.
-- Reads through this query path see only the current tenant's rows
-- unless app.is_platform=true. Cross-tenant resolution lives on the
-- non-RLS auth_routing index (TBD), not here.

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

-- name: UpdateMembershipStatus :exec
UPDATE identity.tenant_memberships
SET    status  = $2,
       left_at = $3
WHERE  id = $1;
