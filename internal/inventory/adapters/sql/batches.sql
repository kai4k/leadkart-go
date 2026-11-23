-- Batch queries — inventory.batches is tenant-scoped + RLS-FORCE.
-- Composite-FK on (product_id, tenant_id) → products(id, tenant_id)
-- guarantees same-tenant linkage at the DB.

-- name: InsertBatch :exec
INSERT INTO inventory.batches (
    id, product_id, tenant_id, batch_number,
    manufacture_date, expiry_date,
    manufacturer_name, manufacturing_licence_number,
    mrp_paise, purchase_price_paise,
    quantity_on_hand, version,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6,
    $7, $8,
    $9, $10,
    $11, $12,
    $13, $14
);

-- name: GetBatchByID :one
SELECT id, product_id, tenant_id, batch_number,
       manufacture_date, expiry_date,
       manufacturer_name, manufacturing_licence_number,
       mrp_paise, purchase_price_paise,
       quantity_on_hand, version,
       created_at, updated_at,
       is_deleted, deleted_at, deleted_by, created_by_membership_id,
       quantity_reserved, quantity_quarantined
FROM   inventory.batches
WHERE  id = $1
AND    NOT is_deleted;

-- name: LockBatchForUpdate :one
-- Pessimistic row-level lock for the UpdateByID path. Concurrent
-- callers block here until the holding tx commits/rolls back. Returns
-- ErrNoRows (→ batch.ErrNotFound) on missing or soft-deleted rows —
-- same visibility filter as GetBatchByID.
SELECT id
FROM   inventory.batches
WHERE  id = $1
AND    NOT is_deleted
FOR UPDATE;

-- name: UpdateBatchWithVersionCheck :execrows
-- Optimistic-concurrency UPDATE. Returns rows-affected so the adapter
-- can branch on 0 → ErrConcurrencyConflict. The `version = $11`
-- predicate is the load-bearing concurrency token; `version + 1` ships
-- the bumped value back.
UPDATE inventory.batches
SET    quantity_on_hand = $2,
       version          = $3,
       updated_at       = $4,
       is_deleted       = $5,
       deleted_at       = $6,
       deleted_by       = $7,
       batch_number     = $8,
       manufacturer_name = $9,
       manufacturing_licence_number = $10,
       quantity_reserved = sqlc.arg(quantity_reserved),
       quantity_quarantined = sqlc.arg(quantity_quarantined)
WHERE  id = $1
AND    version = sqlc.arg(version_expected);

-- name: ListBatchesByProductPage :many
-- Cursor-paginated list per ADR 0038. Keyset on (expiry_date, id) DESC
-- because FEFO ordering = expiry-first per BRD §6.5. Note: ASC for FEFO
-- would be natural, but to keep the canonical "(sort_value, id) <"
-- predicate consistent with the rest of the platform, we order DESC
-- on (expiry_date, id) — the frontend can reverse for FEFO display.
SELECT id, product_id, tenant_id, batch_number,
       manufacture_date, expiry_date,
       manufacturer_name, manufacturing_licence_number,
       mrp_paise, purchase_price_paise,
       quantity_on_hand, version,
       created_at, updated_at,
       is_deleted, deleted_at, deleted_by, created_by_membership_id,
       quantity_reserved, quantity_quarantined
FROM   inventory.batches
WHERE  product_id = $1
AND    NOT is_deleted
AND    (sqlc.arg(include_expired)::boolean OR expiry_date >= sqlc.arg(min_sellable)::date)
AND    (expiry_date, id) < (sqlc.arg(cursor_expiry_date)::date, sqlc.arg(cursor_id)::uuid)
ORDER  BY expiry_date DESC, id DESC
LIMIT  sqlc.arg('limit');

-- name: AnyLiveBatchWithStockForProduct :one
SELECT EXISTS (
    SELECT 1
    FROM   inventory.batches
    WHERE  product_id = $1
    AND    NOT is_deleted
    AND    quantity_on_hand > 0
);

-- name: ListFefoBatchesForProduct :many
-- FEFO (First Expired First Out) ordering per BRD §6.5 — feeds the
-- dispatch picker so warehouse staff pull oldest-expiring inventory
-- first. Filters: live + in-stock + not-yet-expired. Order: expiry_date
-- ASC, id ASC. No pagination — the dispatch picker needs the FULL set.
-- min_sellable is the ONE expiry bound (common/expiry.MinSellableExpiry): a batch
-- is sellable exactly when expiry_date >= min_sellable. Only batches with FREE
-- units (on hand minus reserved minus quarantined) are returned. Column order
-- MUST match inventory.batches so sqlc returns the db.InventoryBatch model.
SELECT id, product_id, tenant_id, batch_number,
       manufacture_date, expiry_date,
       manufacturer_name, manufacturing_licence_number,
       mrp_paise, purchase_price_paise,
       quantity_on_hand, version,
       created_at, updated_at,
       is_deleted, deleted_at, deleted_by, created_by_membership_id,
       quantity_reserved, quantity_quarantined
FROM   inventory.batches
WHERE  product_id = $1
AND    NOT is_deleted
AND    quantity_on_hand - quantity_reserved - quantity_quarantined > 0
AND    expiry_date >= sqlc.arg('min_sellable')::date
ORDER  BY expiry_date ASC, id ASC;

-- name: ListBatchesForExpiryScan :many
-- ExpiryScanJob workhorse. Every live batch of the tenant that still holds units
-- not set apart (on hand minus quarantined > 0), with its product. The scan
-- decides the tier and whether the printed expiry has passed in Go with
-- common/expiry, the one rule; this query only narrows to batches with stock.
-- $1 = tenant_id.
SELECT b.id AS batch_id, b.product_id, b.batch_number, b.expiry_date,
       b.quantity_on_hand, b.quantity_reserved, b.quantity_quarantined,
       p.sku, p.name AS product_name, p.expiry_alert_threshold_days
FROM   inventory.batches b
JOIN   inventory.products p
    ON p.id        = b.product_id
   AND p.tenant_id = b.tenant_id
WHERE  b.tenant_id = $1
AND    NOT b.is_deleted
AND    NOT p.is_deleted
AND    b.quantity_on_hand - b.quantity_quarantined > 0
ORDER  BY b.expiry_date ASC, b.id ASC;

-- name: ListProductsBelowReorderForTenant :many
-- ReorderScanJob workhorse. Per-tenant scan returning products where
--   reorder_level > 0 AND
--   SUM(sellable batches' on-hand units, quarantined excluded) < reorder_level
-- A sellable batch is one with expiry_date >= min_sellable (the one bound).
-- LEFT JOIN so products with NO sellable stock still surface.
-- $1 = tenant_id, min_sellable = common/expiry.MinSellableExpiry.
SELECT p.id, p.sku, p.reorder_level,
       COALESCE(SUM(b.quantity_on_hand - b.quantity_quarantined), 0)::bigint AS stock_on_hand
FROM   inventory.products p
LEFT JOIN inventory.batches b
    ON  b.product_id = p.id
    AND b.tenant_id  = p.tenant_id
    AND NOT b.is_deleted
    AND b.quantity_on_hand - b.quantity_quarantined > 0
    AND b.expiry_date >= sqlc.arg('min_sellable')::date
WHERE  p.tenant_id = $1
AND    NOT p.is_deleted
AND    p.reorder_level > 0
GROUP  BY p.id, p.sku, p.reorder_level
HAVING COALESCE(SUM(b.quantity_on_hand - b.quantity_quarantined), 0) < p.reorder_level
ORDER  BY p.id ASC;
