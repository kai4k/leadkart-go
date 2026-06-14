-- Notification queries — notifications.notifications is tenant-scoped + RLS-FORCE.
-- Recipient isolation is applied in the WHERE by recipient_membership_id; RLS is
-- the tenant-plane backstop.

-- name: InsertNotification :exec
-- Manual / no-source insert (no dedup — manual notifications never dedup).
INSERT INTO notifications.notifications (
    id, tenant_id, recipient_membership_id, category, title, body, state,
    source_module, source_entity_type, source_entity_id, deep_link,
    created_by_membership_id, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13
);

-- name: InsertNotificationDeduped :execrows
-- Sourced insert with a time-windowed dedup probe (at-least-once replay
-- safety): inserts only when no same-source notification to the same recipient
-- exists newer than dedup_cutoff. Returns rows-affected (1 inserted, 0 deduped).
INSERT INTO notifications.notifications (
    id, tenant_id, recipient_membership_id, category, title, body, state,
    source_module, source_entity_type, source_entity_id, deep_link,
    created_by_membership_id, created_at
)
SELECT $1, $2, $3, $4, $5, $6, $7,
       $8, $9, $10, $11,
       $12, $13
WHERE NOT EXISTS (
    SELECT 1
    FROM   notifications.notifications d
    WHERE  d.tenant_id               = $2
    AND    d.recipient_membership_id = $3
    AND    d.source_module           = $8
    AND    d.source_entity_type      = $9
    AND    d.source_entity_id        = $10
    AND    d.category                = $4
    AND    d.created_at > sqlc.arg(dedup_cutoff)
);

-- name: GetNotificationByID :one
SELECT id, tenant_id, recipient_membership_id, category, title, body, state,
       source_module, source_entity_type, source_entity_id, deep_link,
       created_by_membership_id, created_at, read_at, dismissed_at
FROM   notifications.notifications
WHERE  id = $1;

-- name: UpdateNotificationState :exec
UPDATE notifications.notifications
SET    state        = $2,
       read_at      = $3,
       dismissed_at = $4
WHERE  id = $1;

-- name: ListNotificationsForRecipientPage :many
-- Inbox keyset pagination (ADR 0038): newest-first, (created_at, id) DESC.
-- $4 = state filter ('' = no filter), $5 = category filter ('' = no filter).
SELECT id, tenant_id, recipient_membership_id, category, title, body, state,
       source_module, source_entity_type, source_entity_id, deep_link,
       created_by_membership_id, created_at, read_at, dismissed_at
FROM   notifications.notifications
WHERE  tenant_id = $1
AND    recipient_membership_id = $2
AND    (sqlc.arg(state_filter)::text = '' OR state = sqlc.arg(state_filter))
AND    (sqlc.arg(category_filter)::text = '' OR category = sqlc.arg(category_filter))
AND    (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER  BY created_at DESC, id DESC
LIMIT  sqlc.arg('limit');

-- name: CountUnreadForRecipient :one
SELECT COUNT(*)
FROM   notifications.notifications
WHERE  tenant_id = $1
AND    recipient_membership_id = $2
AND    state = 'unread';

-- name: MarkAllUnreadReadForRecipient :execrows
-- Bulk read-all for the inbox. Returns rows-affected.
UPDATE notifications.notifications
SET    state   = 'read',
       read_at = $3
WHERE  tenant_id = $1
AND    recipient_membership_id = $2
AND    state = 'unread';
