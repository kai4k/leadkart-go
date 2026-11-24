-- Batches are never deleted (owner ruling, ADR 0073): stock is corrected by movements (write-off,
-- quarantine, adjustment). Nothing in the code ever set is_deleted on a batch, so the three columns
-- only added a filter to every read; they go, and the partial indexes that carried
-- `WHERE NOT is_deleted` are rebuilt without it.
--
-- The old unique index covered live rows only. A deleted batch that shares its batch number with
-- another batch of the same product keeps its data and takes its own id appended (deterministic,
-- within the 100 characters) before the index covers every row; a batch that was deleted is not
-- hidden any more.

-- +goose Up
-- +goose StatementBegin

UPDATE inventory.batches d
SET    batch_number = left(d.batch_number, 100 - 37) || '~' || d.id::text
WHERE  d.is_deleted
AND    EXISTS (SELECT 1 FROM inventory.batches o
               WHERE o.product_id = d.product_id AND o.batch_number = d.batch_number AND o.id <> d.id);

DROP INDEX IF EXISTS inventory.idx_batches_product;
DROP INDEX IF EXISTS inventory.idx_batches_tenant_expiry;
DROP INDEX IF EXISTS inventory.uq_batches_product_number_live;

ALTER TABLE inventory.batches
    DROP COLUMN is_deleted,
    DROP COLUMN deleted_at,
    DROP COLUMN deleted_by;

CREATE INDEX idx_batches_product
    ON inventory.batches (product_id, expiry_date DESC, id DESC);
CREATE INDEX idx_batches_tenant_expiry
    ON inventory.batches (tenant_id, expiry_date ASC);
CREATE UNIQUE INDEX uq_batches_product_number
    ON inventory.batches (product_id, batch_number);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS inventory.uq_batches_product_number;
DROP INDEX IF EXISTS inventory.idx_batches_tenant_expiry;
DROP INDEX IF EXISTS inventory.idx_batches_product;

ALTER TABLE inventory.batches
    ADD COLUMN is_deleted boolean     NOT NULL DEFAULT false,
    ADD COLUMN deleted_at timestamptz NULL,
    ADD COLUMN deleted_by text        NULL;

CREATE INDEX idx_batches_product
    ON inventory.batches (product_id, expiry_date DESC, id DESC) WHERE NOT is_deleted;
CREATE INDEX idx_batches_tenant_expiry
    ON inventory.batches (tenant_id, expiry_date ASC) WHERE NOT is_deleted;
CREATE UNIQUE INDEX uq_batches_product_number_live
    ON inventory.batches (product_id, batch_number) WHERE NOT is_deleted;

-- +goose StatementEnd
