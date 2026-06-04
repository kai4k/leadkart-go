-- CallLog queries — crm.call_logs is RLS+FORCE per ADR 0006 +
-- migration 20260602000001. Append-only: no UPDATE / DELETE queries.
--
-- Slice A.2 (migration 20260603000602) added callback_window_start_at
-- + callback_window_end_at columns per BRD §4.5. The CallLogged
-- integration event carries them so the Reminder slice's subscriber
-- can mint a callback reminder when the caller stamped a window.

-- name: InsertCallLog :exec
INSERT INTO crm.call_logs (
    id, tenant_id, lead_id, outcome, notes,
    logged_by_membership_id, logged_at, created_at,
    callback_window_start_at, callback_window_end_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetCallLogByID :one
SELECT id, tenant_id, lead_id, outcome, notes,
       logged_by_membership_id, logged_at, created_at,
       callback_window_start_at, callback_window_end_at
FROM   crm.call_logs
WHERE  id = $1;

-- name: ListCallLogsByLead :many
SELECT id, tenant_id, lead_id, outcome, notes,
       logged_by_membership_id, logged_at, created_at,
       callback_window_start_at, callback_window_end_at
FROM   crm.call_logs
WHERE  lead_id = $1
ORDER  BY logged_at DESC, id DESC;
