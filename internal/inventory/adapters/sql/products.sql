-- Product queries — inventory.products is tenant-scoped + RLS-FORCE.
-- All reads/writes run under TxScopeTenant — the `app.tenant_id` GUC
-- is bound by pgxpool's AfterAcquire, then Postgres RLS does the
-- filter. Cross-tenant access surfaces as "row missing" (ErrNotFound).

-- name: InsertProduct :exec
INSERT INTO inventory.products (
    id, tenant_id, sku, name, dosage_form, pack_size, hsn_code,
    gst_rate_bps, manufacturer, is_active, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12
);

-- name: GetProductByID :one
SELECT id, tenant_id, sku, name, dosage_form, pack_size, hsn_code,
       gst_rate_bps, manufacturer, is_active,
       created_at, updated_at,
       is_deleted, deleted_at, deleted_by, created_by_membership_id
FROM   inventory.products
WHERE  id = $1
AND    NOT is_deleted;

-- name: UpdateProduct :exec
-- General-purpose update covering Product.Update mutator + SoftDelete.
-- The aggregate persists ALL its current state on every write; the
-- repo doesn't try to compute a partial UPDATE.
UPDATE inventory.products
SET    name         = $2,
       gst_rate_bps = $3,
       manufacturer = $4,
       is_active    = $5,
       updated_at   = $6
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
       created_at, updated_at,
       is_deleted, deleted_at, deleted_by, created_by_membership_id
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
