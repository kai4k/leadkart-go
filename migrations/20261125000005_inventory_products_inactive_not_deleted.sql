-- Products go Inactive, never deleted (owner ruling 2026-11-24, ADR 0073).
--
-- The soft-delete columns only served the delete route; is_active replaces them. Every partial index
-- that carried `WHERE NOT is_deleted` is rebuilt without it (dropping the column would drop them).
-- SKU uniqueness is now over every product of the company: a SKU is never freed by "deleting".

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS inventory.uq_products_tenant_sku_live;
DROP INDEX IF EXISTS inventory.idx_products_tenant_created_keyset;
DROP INDEX IF EXISTS inventory.idx_products_tenant_active;
DROP INDEX IF EXISTS inventory.idx_products_search_trgm;
DROP INDEX IF EXISTS inventory.idx_products_tenant_reorder_enabled;
DROP INDEX IF EXISTS inventory.idx_products_schedule_class;

-- A product soft-deleted before this migration is INACTIVE afterwards (it never reappears as active).
UPDATE inventory.products SET is_active = false WHERE is_deleted;

-- The old unique index only covered live rows, so a soft-deleted product may share its SKU with a
-- live one (or with another deleted one). The new index covers every row: each such deleted row
-- keeps its data and takes a deterministic new SKU (its own id appended, within the 64 characters).
UPDATE inventory.products d
SET    sku = left(d.sku, 64 - 37) || '~' || d.id::text
WHERE  d.is_deleted
AND    EXISTS (SELECT 1 FROM inventory.products o
               WHERE o.tenant_id = d.tenant_id AND o.sku = d.sku AND o.id <> d.id);

ALTER TABLE inventory.products
    DROP COLUMN is_deleted,
    DROP COLUMN deleted_at,
    DROP COLUMN deleted_by;

CREATE UNIQUE INDEX uq_products_tenant_sku
    ON inventory.products (tenant_id, sku);

CREATE INDEX idx_products_tenant_created_keyset
    ON inventory.products (tenant_id, created_at DESC, id DESC);

CREATE INDEX idx_products_tenant_active
    ON inventory.products (tenant_id, created_at DESC, id DESC)
    WHERE is_active;

CREATE INDEX idx_products_search_trgm
    ON inventory.products
    USING gin ((lower(sku) || ' ' || lower(name)) gin_trgm_ops);

CREATE INDEX idx_products_tenant_reorder_enabled
    ON inventory.products (tenant_id, id)
    WHERE is_active AND reorder_level > 0;

CREATE INDEX idx_products_schedule_class
    ON inventory.products (tenant_id, drug_schedule_class_id)
    WHERE drug_schedule_class_id IS NOT NULL;

COMMENT ON COLUMN inventory.products.reorder_level IS
    'When SUM(batches.quantity_on_hand, quarantined excluded, of sellable batches) < reorder_level, the daily ReorderScanJob emits ProductBelowReorderLevelV1 for an ACTIVE product. 0 disables the alert (BRD §6.5).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

COMMENT ON COLUMN inventory.products.reorder_level IS
    'When SUM(batches.quantity_on_hand WHERE NOT is_deleted AND NOT expired) < reorder_level, the daily ReorderScanJob emits ProductBelowReorderLevelV1. 0 disables the alert (BRD §6.5).';

DROP INDEX IF EXISTS inventory.idx_products_schedule_class;
DROP INDEX IF EXISTS inventory.idx_products_tenant_reorder_enabled;
DROP INDEX IF EXISTS inventory.idx_products_search_trgm;
DROP INDEX IF EXISTS inventory.idx_products_tenant_active;
DROP INDEX IF EXISTS inventory.idx_products_tenant_created_keyset;
DROP INDEX IF EXISTS inventory.uq_products_tenant_sku;

ALTER TABLE inventory.products
    ADD COLUMN is_deleted boolean     NOT NULL DEFAULT false,
    ADD COLUMN deleted_at timestamptz NULL,
    ADD COLUMN deleted_by text        NULL;

CREATE UNIQUE INDEX uq_products_tenant_sku_live
    ON inventory.products (tenant_id, sku) WHERE NOT is_deleted;
CREATE INDEX idx_products_tenant_created_keyset
    ON inventory.products (tenant_id, created_at DESC, id DESC) WHERE NOT is_deleted;
CREATE INDEX idx_products_tenant_active
    ON inventory.products (tenant_id, created_at DESC, id DESC) WHERE NOT is_deleted AND is_active;
CREATE INDEX idx_products_search_trgm
    ON inventory.products USING gin ((lower(sku) || ' ' || lower(name)) gin_trgm_ops) WHERE NOT is_deleted;
CREATE INDEX idx_products_tenant_reorder_enabled
    ON inventory.products (tenant_id, id) WHERE NOT is_deleted AND reorder_level > 0;
CREATE INDEX idx_products_schedule_class
    ON inventory.products (tenant_id, drug_schedule_class_id)
    WHERE drug_schedule_class_id IS NOT NULL AND NOT is_deleted;

-- +goose StatementEnd
