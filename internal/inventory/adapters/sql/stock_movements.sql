-- StockMovement queries — inventory.stock_movements is tenant-scoped +
-- RLS-FORCE. Append-only ledger: NO UPDATE, NO DELETE queries.

-- name: InsertStockMovement :exec
INSERT INTO inventory.stock_movements (
    id, batch_id, product_id, tenant_id,
    type, quantity, quantity_on_hand_after,
    reason, actor_membership_id, source_reference,
    occurred_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7,
    $8, $9, $10,
    $11
);

-- name: GetStockMovementByID :one
SELECT id, batch_id, product_id, tenant_id,
       type, quantity, quantity_on_hand_after,
       reason, actor_membership_id, source_reference,
       occurred_at
FROM   inventory.stock_movements
WHERE  id = $1;

-- name: ListMovementsByBatchPage :many
-- Cursor-paginated ledger read per ADR 0038. Keyset on (occurred_at, id) DESC.
-- $4 = type filter (empty = no filter).
-- $5 = page_size + 1 (peek-one-extra).
SELECT id, batch_id, product_id, tenant_id,
       type, quantity, quantity_on_hand_after,
       reason, actor_membership_id, source_reference,
       occurred_at
FROM   inventory.stock_movements
WHERE  batch_id = $1
AND    (sqlc.arg(type)::text = '' OR type = sqlc.arg(type))
AND    (occurred_at, id) < (sqlc.arg(cursor_occurred_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER  BY occurred_at DESC, id DESC
LIMIT  sqlc.arg('limit');
