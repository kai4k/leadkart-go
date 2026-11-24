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

-- name: CountReservationsForSource :one
-- True-hold idempotency probe (ADR 0063 §4). Returns the number of
-- reservation rows logged against an order (source_reference). The reserve
-- handler no-ops when this is > 0 — an at-least-once redelivery of
-- orders.order_confirmed.v1 must not double-reserve.
SELECT COUNT(*)
FROM   inventory.stock_movements
WHERE  tenant_id = $1
AND    source_reference = sqlc.arg(source_reference)
AND    type = 'reservation';

-- name: ListOutstandingReservationsForSource :many
-- Net outstanding reserved quantity per batch for an order (source_reference):
--   net = Σ reservation − Σ release
-- (Outbound rows are excluded — they reduce on-hand, not the reserved hold.)
-- Returns only batches still holding > 0, which doubles as the release/consume
-- idempotency signal: a fully-released or fully-consumed order yields zero rows
-- → the handler no-ops. product_id rides along for movement construction.
SELECT batch_id,
       product_id,
       COALESCE(SUM(
           CASE type
               WHEN 'reservation' THEN quantity
               WHEN 'release'     THEN -quantity
               ELSE 0
           END), 0)::bigint AS net_reserved
FROM   inventory.stock_movements
WHERE  tenant_id = $1
AND    source_reference = sqlc.arg(source_reference)
GROUP  BY batch_id, product_id
HAVING COALESCE(SUM(
           CASE type
               WHEN 'reservation' THEN quantity
               WHEN 'release'     THEN -quantity
               ELSE 0
           END), 0) > 0;

-- name: CountExpiredBatchesHeldByOrder :one
-- How many batches the order still holds units on whose printed expiry month is
-- over on the business day (expiry_date is the last day of the printed month, so
-- "past" is expiry_date < today). Held = Σ reservation − Σ release > 0, the same
-- rule as ListOutstandingReservationsForSource. Orders asks this before it lets
-- an order be packed: expired stock is never sold.
SELECT count(*)::bigint
FROM   inventory.batches b
WHERE  b.tenant_id = $1
AND    b.expiry_date < sqlc.arg(today)::date
AND    b.id IN (
           SELECT m.batch_id
           FROM   inventory.stock_movements m
           WHERE  m.tenant_id = $1
           AND    m.source_reference = sqlc.arg(source_reference)
           GROUP  BY m.batch_id
           HAVING COALESCE(SUM(
                      CASE m.type
                          WHEN 'reservation' THEN m.quantity
                          WHEN 'release'     THEN -m.quantity
                          ELSE 0
                      END), 0) > 0
       );
