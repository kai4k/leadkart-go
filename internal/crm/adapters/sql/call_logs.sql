-- CallLog queries — crm.call_logs is RLS+FORCE per ADR 0006 +
-- migration 20260602000001. Append-only: no UPDATE / DELETE queries.

-- name: InsertCallLog :exec
INSERT INTO crm.call_logs (
    id, tenant_id, lead_id, outcome, notes,
    logged_by_membership_id, logged_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetCallLogByID :one
SELECT id, tenant_id, lead_id, outcome, notes,
       logged_by_membership_id, logged_at, created_at
FROM   crm.call_logs
WHERE  id = $1;

-- name: ListCallLogsByLead :many
SELECT id, tenant_id, lead_id, outcome, notes,
       logged_by_membership_id, logged_at, created_at
FROM   crm.call_logs
WHERE  lead_id = $1
ORDER  BY logged_at DESC, id DESC;
