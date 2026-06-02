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
       is_deleted, deleted_at, deleted_by, created_by_membership_id
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
       manufacturing_licence_number = $10
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
       is_deleted, deleted_at, deleted_by, created_by_membership_id
FROM   inventory.batches
WHERE  product_id = $1
AND    NOT is_deleted
AND    (sqlc.arg(include_expired)::boolean OR expiry_date > now())
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
