-- WorkItem queries — tasks.work_items is RLS+FORCE per ADR 0006 +
-- migration 20260604000001. Reads/writes flow through RLS: only the
-- current tenant's rows are visible unless app.is_platform=true.
--
-- Slice 1 surfaces:
--   InsertWorkItem            → repo Add path
--   GetWorkItemByID           → repo GetByID + UpdateByID load path
--   GetOpenWorkItemBySource   → auto-complete-by-source lookup
--   UpdateWorkItem            → repo UpdateByID persist path
--   ListWorkItemsPage         → cursor-paginated list per ADR 0038
--   ListOverdueCandidates     → periodic overdue-scan job
--   ListPurgeCandidates       → daily purge job
--   SoftDeleteWorkItem        → purge job's tombstone flip
--   DashboardCounts           → per-membership / per-team tallies

-- name: InsertWorkItem :exec
INSERT INTO tasks.work_items (
    id, tenant_id, type, priority, state,
    title, description,
    assigned_to_membership_id, assigned_by_membership_id,
    due_at, completed_at, cancelled_at, cancellation_reason,
    batch_id, source_module, source_entity_type, source_entity_id,
    created_at, created_by_membership_id
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7,
    $8, $9,
    $10, $11, $12, $13,
    $14, $15, $16, $17,
    $18, $19
);

-- name: GetWorkItemByID :one
SELECT id, tenant_id, type, priority, state,
       title, description,
       assigned_to_membership_id, assigned_by_membership_id,
       due_at, completed_at, cancelled_at, cancellation_reason,
       batch_id, source_module, source_entity_type, source_entity_id,
       created_at, created_by_membership_id
FROM   tasks.work_items
WHERE  id = $1
  AND  NOT is_deleted;

-- name: GetOpenWorkItemBySource :one
SELECT id, tenant_id, type, priority, state,
       title, description,
       assigned_to_membership_id, assigned_by_membership_id,
       due_at, completed_at, cancelled_at, cancellation_reason,
       batch_id, source_module, source_entity_type, source_entity_id,
       created_at, created_by_membership_id
FROM   tasks.work_items
WHERE  source_entity_type = $1
  AND  source_entity_id   = $2
  AND  state IN ('pending', 'in_progress')
  AND  NOT is_deleted
LIMIT  1;

-- name: UpdateWorkItem :exec
-- Persists the mutable WorkItem state. tenant_id + type + source_* +
-- created_at + created_by_membership_id are aggregate-immutable.
UPDATE tasks.work_items
SET    priority                  = $2,
       state                     = $3,
       title                     = $4,
       description               = $5,
       assigned_to_membership_id = $6,
       due_at                    = $7,
       completed_at              = $8,
       cancelled_at              = $9,
       cancellation_reason       = $10
WHERE  id = $1
  AND  NOT is_deleted;

-- name: SoftDeleteWorkItem :exec
UPDATE tasks.work_items
SET    is_deleted = true
WHERE  id = $1
  AND  NOT is_deleted;

-- name: ListWorkItemsPage :many
-- Cursor (keyset) pagination on (due_at, id) DESC per ADR 0038.
-- Composite filter columns supplied as nullable params.
SELECT id, tenant_id, type, priority, state,
       title, description,
       assigned_to_membership_id, assigned_by_membership_id,
       due_at, completed_at, cancelled_at, cancellation_reason,
       batch_id, source_module, source_entity_type, source_entity_id,
       created_at, created_by_membership_id
FROM   tasks.work_items
WHERE  tenant_id = $1
  AND  NOT is_deleted
  AND  (sqlc.narg('state')::text    IS NULL OR state    = sqlc.narg('state')::text)
  AND  (sqlc.narg('type')::text     IS NULL OR type     = sqlc.narg('type')::text)
  AND  (sqlc.narg('priority')::text IS NULL OR priority = sqlc.narg('priority')::text)
  AND  (sqlc.narg('assignee')::uuid IS NULL OR assigned_to_membership_id = sqlc.narg('assignee')::uuid)
  AND  (sqlc.narg('self_assignee')::uuid IS NULL OR assigned_to_membership_id = sqlc.narg('self_assignee')::uuid)
  AND  (sqlc.narg('batch_id')::uuid IS NULL OR batch_id = sqlc.narg('batch_id')::uuid)
  AND  (sqlc.narg('due_before')::timestamptz IS NULL OR due_at <  sqlc.narg('due_before')::timestamptz)
  AND  (sqlc.narg('due_after')::timestamptz  IS NULL OR due_at >  sqlc.narg('due_after')::timestamptz)
  AND  (sqlc.narg('cursor_due_at')::timestamptz IS NULL OR
        (due_at, id) < (sqlc.narg('cursor_due_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER  BY due_at DESC, id DESC
LIMIT  sqlc.arg('page_size')::int;

-- name: ListOverdueCandidates :many
-- Cross-tenant scan for the periodic overdue-scan job. The forwarder /
-- job runs under TxScopePlatform so RLS sees all tenants; the optional
-- tenant_id narg constrains to one tenant for testing.
SELECT id, tenant_id, type, priority, state,
       title, description,
       assigned_to_membership_id, assigned_by_membership_id,
       due_at, completed_at, cancelled_at, cancellation_reason,
       batch_id, source_module, source_entity_type, source_entity_id,
       created_at, created_by_membership_id
FROM   tasks.work_items
WHERE  NOT is_deleted
  AND  state IN ('pending', 'in_progress')
  AND  due_at < sqlc.arg('as_of')::timestamptz
  AND  (sqlc.narg('tenant_id')::uuid IS NULL OR tenant_id = sqlc.narg('tenant_id')::uuid)
ORDER  BY due_at ASC
LIMIT  sqlc.arg('row_limit')::int;

-- name: ListPurgeCandidates :many
-- Cross-tenant scan for the daily purge job. Terminal rows whose
-- terminal timestamp predates `before` are returned.
SELECT id, tenant_id, type, priority, state,
       title, description,
       assigned_to_membership_id, assigned_by_membership_id,
       due_at, completed_at, cancelled_at, cancellation_reason,
       batch_id, source_module, source_entity_type, source_entity_id,
       created_at, created_by_membership_id
FROM   tasks.work_items
WHERE  NOT is_deleted
  AND  state IN ('completed', 'cancelled')
  AND  COALESCE(completed_at, cancelled_at) < sqlc.arg('before')::timestamptz
  AND  (sqlc.narg('tenant_id')::uuid IS NULL OR tenant_id = sqlc.narg('tenant_id')::uuid)
LIMIT  sqlc.arg('row_limit')::int;

-- name: DashboardCounts :one
-- Single-query CTE produces the BRD §6.8 dashboard counts:
--   today           — open tasks due on the same UTC day as as_of
--   upcoming        — open tasks due after as_of's UTC day
--   overdue         — tasks in StateOverdue
--   completed_today — tasks completed on the same UTC day as as_of
--   total_pending   — tasks in StatePending
--
-- visible_membership_ids is an optional uuid[] — when non-NULL the
-- counts span only those memberships (team view); when NULL the counts
-- span every membership in the tenant.
WITH scoped AS (
    SELECT state, due_at, completed_at, assigned_to_membership_id
    FROM   tasks.work_items
    WHERE  tenant_id = sqlc.arg('tenant_id')::uuid
      AND  NOT is_deleted
      AND  (sqlc.narg('visible_membership_ids')::uuid[] IS NULL
            OR assigned_to_membership_id = ANY(sqlc.narg('visible_membership_ids')::uuid[]))
)
SELECT
    COUNT(*) FILTER (
        WHERE state IN ('pending', 'in_progress')
          AND date_trunc('day', due_at AT TIME ZONE 'UTC')
              = date_trunc('day', sqlc.arg('as_of')::timestamptz AT TIME ZONE 'UTC')
    ) AS today,
    COUNT(*) FILTER (
        WHERE state IN ('pending', 'in_progress')
          AND date_trunc('day', due_at AT TIME ZONE 'UTC')
              > date_trunc('day', sqlc.arg('as_of')::timestamptz AT TIME ZONE 'UTC')
    ) AS upcoming,
    COUNT(*) FILTER (WHERE state = 'overdue') AS overdue,
    COUNT(*) FILTER (
        WHERE state = 'completed'
          AND completed_at IS NOT NULL
          AND date_trunc('day', completed_at AT TIME ZONE 'UTC')
              = date_trunc('day', sqlc.arg('as_of')::timestamptz AT TIME ZONE 'UTC')
    ) AS completed_today,
    COUNT(*) FILTER (WHERE state = 'pending') AS total_pending
FROM scoped;
