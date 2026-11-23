-- LeadKart Go — Inventory G2: the remaining product fields.
--
--   gst_rate_id             the entry of the company's GST rate list the product
--                           uses (companysettings, ADR 0070). The rate itself stays
--                           in gst_rate_bps; there is no default rate and no
--                           category fallback. NULL only for a product that predates
--                           the list; the API requires it on create.
--   drug_schedule_class_id  the company's own drug schedule class (an id from
--                           drug_schedule.classes). NULLABLE and open: there is no
--                           CHECK and no list of class values here, because which
--                           classes exist is company data, not code.
--   default_sale_rate_paise the rate offered on a quotation line (0: none).
--   generic_name, composition, storage_condition: optional free text.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE inventory.products
    ADD COLUMN gst_rate_id              uuid   NULL,
    ADD COLUMN default_sale_rate_paise  bigint NOT NULL DEFAULT 0 CHECK (default_sale_rate_paise >= 0),
    ADD COLUMN generic_name             text   NOT NULL DEFAULT '' CHECK (length(generic_name) <= 200),
    ADD COLUMN composition              text   NOT NULL DEFAULT '' CHECK (length(composition) <= 1000),
    ADD COLUMN storage_condition        text   NOT NULL DEFAULT '' CHECK (length(storage_condition) <= 200),
    ADD COLUMN drug_schedule_class_id   uuid   NULL;

-- "Is any product still using this class" answers the removal of a class.
CREATE INDEX idx_products_schedule_class
    ON inventory.products (tenant_id, drug_schedule_class_id)
    WHERE drug_schedule_class_id IS NOT NULL AND NOT is_deleted;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS inventory.idx_products_schedule_class;
ALTER TABLE inventory.products
    DROP COLUMN drug_schedule_class_id,
    DROP COLUMN storage_condition,
    DROP COLUMN composition,
    DROP COLUMN generic_name,
    DROP COLUMN default_sale_rate_paise,
    DROP COLUMN gst_rate_id;

-- +goose StatementEnd
