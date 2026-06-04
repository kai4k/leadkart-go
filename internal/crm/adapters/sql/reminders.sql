-- Reminder queries — crm.reminders is RLS+FORCE per slice A.2
-- migration 20260603000601. RLS bind happens at WithinTxPgxTenant —
-- queries here are tenant-blind.
--
-- Slice surfaces:
--   InsertReminder              → repo Add (subscriber / cron / HTTP)
--   GetReminderByID             → repo GetByID + UpdateByID load path
--   UpdateReminderState         → repo UpdateByID persist path
--                                   (rewrites the mutable cols)
--   ListPendingRemindersPage    → cursor-paginated pending list per ADR 0038
--   FindPendingMatureForLead    → mature-lead idempotency probe
--   ListConvertedLeadsForMatureScan → mature-lead daily scan input
--
-- mature-lead scan v0.2 approximation: the BRD says "no reorder in 3
-- months" but crmlead doesn't track reorders at slice 1; the v0.2
-- approximation uses converted_at < cutoff. v0.3 wires actual reorder
-- tracking.

-- name: InsertReminder :exec
INSERT INTO crm.reminders (
    id, tenant_id, lead_id, assigned_to_membership_id,
    created_by_membership_id, source_call_log_id,
    type, state,
    due_at, notes,
    sent_at, marked_sent_by_membership_id,
    cancelled_at, cancelled_by_membership_id, cancel_reason,
    created_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6,
    $7, $8,
    $9, $10,
    $11, $12,
    $13, $14, $15,
    $16
);

-- name: GetReminderByID :one
SELECT id, tenant_id, lead_id, assigned_to_membership_id,
       created_by_membership_id, source_call_log_id,
       type, state,
       due_at, notes,
       sent_at, marked_sent_by_membership_id,
       cancelled_at, cancelled_by_membership_id, cancel_reason,
       created_at
FROM   crm.reminders
WHERE  id = $1;

-- name: UpdateReminderState :exec
-- Rewrites the mutable lifecycle columns. tenant_id + lead_id +
-- assigned_to + created_by + source_call_log_id + type + due_at +
-- notes + created_at are aggregate-immutable — this query
-- intentionally does NOT write them.
UPDATE crm.reminders
SET    state                       = $2,
       sent_at                     = $3,
       marked_sent_by_membership_id = $4,
       cancelled_at                = $5,
       cancelled_by_membership_id  = $6,
       cancel_reason               = $7
WHERE  id = $1;

-- name: ListPendingRemindersPage :many
-- Cursor (keyset) pagination on (due_at ASC, id ASC) per ADR 0038. Hot
-- "today / upcoming / overdue" path uses the partial index
-- idx_crm_reminders_pending_assignee_due — overdue rows surface first
-- by virtue of the ASC ordering.
--
-- LIMIT is $page_size + 1 (peek-one-extra) — adapter strips the extra
-- row + sets HasMore based on the returned row count.
SELECT id, tenant_id, lead_id, assigned_to_membership_id,
       created_by_membership_id, source_call_log_id,
       type, state,
       due_at, notes,
       sent_at, marked_sent_by_membership_id,
       cancelled_at, cancelled_by_membership_id, cancel_reason,
       created_at
FROM   crm.reminders
WHERE  tenant_id = $1
  AND  state = 'pending'
  AND  (sqlc.narg('assignee')::uuid IS NULL OR assigned_to_membership_id = sqlc.narg('assignee')::uuid)
  AND  (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
  AND  (sqlc.narg('lead_id')::uuid IS NULL OR lead_id = sqlc.narg('lead_id')::uuid)
  AND  (sqlc.narg('cursor_due_at')::timestamptz IS NULL OR
        (due_at, id) > (sqlc.narg('cursor_due_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER  BY due_at ASC, id ASC
LIMIT  sqlc.arg('page_size')::int;

-- name: FindPendingMatureForLead :one
-- Hot-path probe for the mature-lead scheduler — saves the round-trip
-- of catching a SQLSTATE 23505 on the partial unique index. Returns at
-- most one row (the index enforces it).
SELECT id, tenant_id, lead_id, assigned_to_membership_id,
       created_by_membership_id, source_call_log_id,
       type, state,
       due_at, notes,
       sent_at, marked_sent_by_membership_id,
       cancelled_at, cancelled_by_membership_id, cancel_reason,
       created_at
FROM   crm.reminders
WHERE  tenant_id = $1
  AND  lead_id   = $2
  AND  type      = 'mature_lead'
  AND  state     = 'pending';

-- name: ListConvertedLeadsForMatureScanAllTenants :many
-- Cross-tenant variant for the daily scan running under TxScopePlatform.
-- Returns (tenant_id, lead_id, assignee, contact_name, converted_at)
-- for every converted lead older than the cutoff. The river worker
-- runs once per day for the whole installation; using a single
-- platform-scoped query avoids a per-tenant tx round-trip when most
-- tenants have no qualifying leads.
SELECT id, tenant_id, assignee_membership_id, contact_name, converted_at
FROM   crm.crm_leads
WHERE  stage       = 'converted'
  AND  converted_at IS NOT NULL
  AND  converted_at < $1
  AND  assignee_membership_id IS NOT NULL
ORDER  BY tenant_id, converted_at ASC, id ASC
LIMIT  $2;
