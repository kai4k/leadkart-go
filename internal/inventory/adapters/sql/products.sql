-- Product queries — inventory.products is tenant-scoped + RLS-FORCE.
-- All reads/writes run under TxScopeTenant — the `app.tenant_id` GUC
-- is bound by pgxpool's AfterAcquire, then Postgres RLS does the
-- filter. Cross-tenant access surfaces as "row missing" (ErrNotFound).

-- name: InsertProduct :exec
INSERT INTO inventory.products (
    id, tenant_id, sku, name, dosage_form, pack_size, hsn_code,
    gst_rate_bps, manufacturer, is_active, created_at, updated_at,
    reorder_level, expiry_alert_threshold_days, product_category
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12,
    $13, $14, $15
);

-- name: GetProductByID :one
-- Column order MUST match the inventory.products table column order so sqlc
-- returns the db.InventoryProduct model (not a custom row).
SELECT id, tenant_id, sku, name, dosage_form, pack_size, hsn_code,
       gst_rate_bps, manufacturer, is_active,
       created_at, updated_at, is_deleted, deleted_at, deleted_by,
       created_by_membership_id, reorder_level, expiry_alert_threshold_days,
       product_category
FROM   inventory.products
WHERE  id = $1
AND    NOT is_deleted;

-- name: UpdateProduct :exec
-- General-purpose update covering Product.Update mutator + SoftDelete.
-- The aggregate persists ALL its current state on every write; the
-- repo doesn't try to compute a partial UPDATE.
UPDATE inventory.products
SET    name                        = $2,
       gst_rate_bps                = $3,
       manufacturer                = $4,
       is_active                   = $5,
       updated_at                  = $6,
       reorder_level               = $7,
       expiry_alert_threshold_days = $8,
       product_category            = $9
WHERE  id = $1
AND    NOT is_deleted;

-- name: SoftDeleteProduct :exec
UPDATE inventory.products
SET    is_deleted = true,
       deleted_at = $2,
       deleted_by = $3,
       updated_at = $4
WHERE  id = $1
AND    NOT is_deleted;

-- name: ListProductsByTenantPage :many
-- Cursor-paginated list per ADR 0038. Keyset on (created_at, id) DESC.
-- The cursor's `$2` = sort_value (created_at), `$3` = id; first page
-- supplies (clock-max-future, max-uuid) so the predicate matches everything.
-- $4 = active_only flag. $5 = search predicate (empty = no filter).
-- $6 = page_size + 1 (peek-one-extra per pagination.BuildPage).
SELECT id, tenant_id, sku, name, dosage_form, pack_size, hsn_code,
       gst_rate_bps, manufacturer, is_active,
       created_at, updated_at, is_deleted, deleted_at, deleted_by,
       created_by_membership_id, reorder_level, expiry_alert_threshold_days,
       product_category
FROM   inventory.products
WHERE  tenant_id = $1
AND    NOT is_deleted
AND    (NOT sqlc.arg(active_only)::boolean OR is_active = true)
AND    (
        sqlc.arg(search)::text = ''
        OR (lower(sku) || ' ' || lower(name)) ILIKE '%' || lower(sqlc.arg(search)) || '%'
       )
AND    (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER  BY created_at DESC, id DESC
LIMIT  sqlc.arg('limit');

-- name: GetProductStockByID :one
-- Read model for the product detail: the product row plus its stock totals,
-- in ONE statement (no per-product follow-up read). The totals count LIVE
-- batches that are NOT expired (expiry_date > today, the rule the FEFO picker
-- and the reorder scan use): units in an expired batch are not stock.
-- earliest_expiry is the soonest expiry among those batches that hold stock;
-- NULL when none does. RLS scopes both tables to the caller's tenant.
SELECT p.id, p.tenant_id, p.sku, p.name, p.dosage_form, p.pack_size, p.hsn_code,
       p.gst_rate_bps, p.manufacturer, p.is_active, p.created_at, p.updated_at,
       p.reorder_level, p.expiry_alert_threshold_days, p.product_category,
       COALESCE(s.stock_on_hand, 0)::bigint  AS stock_on_hand,
       COALESCE(s.stock_reserved, 0)::bigint AS stock_reserved,
       s.earliest_expiry::date               AS earliest_expiry
FROM   inventory.products p
LEFT   JOIN LATERAL (
           SELECT SUM(b.quantity_on_hand)  AS stock_on_hand,
                  SUM(b.quantity_reserved) AS stock_reserved,
                  MIN(b.expiry_date) FILTER (WHERE b.quantity_on_hand > 0) AS earliest_expiry
           FROM   inventory.batches b
           WHERE  b.product_id = p.id
           AND    b.tenant_id  = p.tenant_id
           AND    NOT b.is_deleted
           AND    b.expiry_date > sqlc.arg('today')::date
       ) s ON true
WHERE  p.id = sqlc.arg('id')
AND    NOT p.is_deleted;

-- name: ListProductStockPage :many
-- Read model for the product list: one statement returns the page WITH each
-- product's stock totals (same definitions as GetProductStockByID), so the
-- list never issues a query per row. Keyset on (created_at, id) DESC per
-- ADR 0038, as ListProductsByTenantPage.
--
-- Filters (each one is off at its neutral value):
--   active_only          false
--   search               ''   substring of "sku name", case-insensitive
--   category             ''   exact product_category, case-insensitive
--   dosage_form          ''   exact, case-insensitive
--   manufacturer         ''   exact, case-insensitive
--   low_stock            false; true = reorder_level > 0 AND stock_on_hand <
--                        reorder_level (the reorder scan's rule)
--   expiring_within_days -1;  n >= 0 = a live, in-stock, not-expired batch
--                        expires on or before today + n days
SELECT p.id, p.tenant_id, p.sku, p.name, p.dosage_form, p.pack_size, p.hsn_code,
       p.gst_rate_bps, p.manufacturer, p.is_active, p.created_at, p.updated_at,
       p.reorder_level, p.expiry_alert_threshold_days, p.product_category,
       COALESCE(s.stock_on_hand, 0)::bigint  AS stock_on_hand,
       COALESCE(s.stock_reserved, 0)::bigint AS stock_reserved,
       s.earliest_expiry::date               AS earliest_expiry
FROM   inventory.products p
LEFT   JOIN LATERAL (
           SELECT SUM(b.quantity_on_hand)  AS stock_on_hand,
                  SUM(b.quantity_reserved) AS stock_reserved,
                  MIN(b.expiry_date) FILTER (WHERE b.quantity_on_hand > 0) AS earliest_expiry
           FROM   inventory.batches b
           WHERE  b.product_id = p.id
           AND    b.tenant_id  = p.tenant_id
           AND    NOT b.is_deleted
           AND    b.expiry_date > sqlc.arg('today')::date
       ) s ON true
WHERE  p.tenant_id = sqlc.arg('tenant_id')
AND    NOT p.is_deleted
AND    (NOT sqlc.arg('active_only')::boolean OR p.is_active = true)
AND    (
        sqlc.arg('search')::text = ''
        OR (lower(p.sku) || ' ' || lower(p.name)) ILIKE '%' || lower(sqlc.arg('search')) || '%'
       )
AND    (sqlc.arg('category')::text = '' OR lower(p.product_category) = lower(sqlc.arg('category')))
AND    (sqlc.arg('dosage_form')::text = '' OR lower(p.dosage_form) = lower(sqlc.arg('dosage_form')))
AND    (sqlc.arg('manufacturer')::text = '' OR lower(p.manufacturer) = lower(sqlc.arg('manufacturer')))
AND    (
        NOT sqlc.arg('low_stock')::boolean
        OR (p.reorder_level > 0 AND COALESCE(s.stock_on_hand, 0) < p.reorder_level)
       )
AND    (
        sqlc.arg('expiring_within_days')::int < 0
        OR s.earliest_expiry <= sqlc.arg('today')::date + sqlc.arg('expiring_within_days')::int
       )
AND    (p.created_at, p.id) < (sqlc.arg('cursor_created_at')::timestamptz, sqlc.arg('cursor_id')::uuid)
ORDER  BY p.created_at DESC, p.id DESC
LIMIT  sqlc.arg('limit');
