-- LeadKart Go — Inventory Slice 1 fix-pass: composite-index direction
-- alignment with query ORDER BY (ADR 0061 amendment 1, finding M7).
--
-- The original migration 20260603000001 declared
--   idx_batches_product (product_id, expiry_date ASC, id DESC) WHERE NOT is_deleted
-- while the query `ListBatchesByProductPage` orders
--   (expiry_date DESC, id DESC)
--
-- Postgres can backward-scan an ASC index to satisfy a DESC ORDER BY at
-- full performance (B-trees are symmetrically walkable), so this was a
-- documentation drift, not a measured perf regression. The fix-pass
-- aligns the index to the query so an EXPLAIN reader doesn't get
-- confused and so the regression-test plan-shape stays
-- "Index Scan Backward" or "Index Scan" identically.
--
-- DROP + CREATE rather than ALTER — Postgres doesn't support
-- re-ordering an existing index. WHERE NOT is_deleted preserved.

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS inventory.idx_batches_product;
CREATE INDEX idx_batches_product
    ON inventory.batches (product_id, expiry_date DESC, id DESC)
    WHERE NOT is_deleted;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS inventory.idx_batches_product;
CREATE INDEX idx_batches_product
    ON inventory.batches (product_id, expiry_date ASC, id DESC)
    WHERE NOT is_deleted;

-- +goose StatementEnd
