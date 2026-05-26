-- AssignmentHistory queries — crm.assignment_history is RLS+FORCE per
-- ADR 0006 + migration 20260602000001. Append-only: no UPDATE / DELETE.

-- name: InsertAssignmentHistory :exec
INSERT INTO crm.assignment_history (
    id, tenant_id, lead_id,
    previous_assignee_membership_id, assignee_membership_id,
    assigned_by_membership_id, reason, assigned_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetAssignmentHistoryByID :one
SELECT id, tenant_id, lead_id,
       previous_assignee_membership_id, assignee_membership_id,
       assigned_by_membership_id, reason, assigned_at, created_at
FROM   crm.assignment_history
WHERE  id = $1;

-- name: ListAssignmentHistoryByLead :many
SELECT id, tenant_id, lead_id,
       previous_assignee_membership_id, assignee_membership_id,
       assigned_by_membership_id, reason, assigned_at, created_at
FROM   crm.assignment_history
WHERE  lead_id = $1
ORDER  BY assigned_at DESC, id DESC;
