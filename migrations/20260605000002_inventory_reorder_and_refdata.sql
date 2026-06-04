-- LeadKart Go — Inventory Phase A.3:
--   1. inventory.products columns: reorder_level, expiry_alert_threshold_days,
--      product_category (default-GST driver per BRD §6.5 + Appendix C.5).
--   2. shared.product_category_gst_defaults reference table (BRD App C.5).
--   3. inventory.alert_emissions dedup table for the expiry + reorder
--      scan river jobs (per-tenant + per-kind + per-subject + per-day
--      idempotency key).
--
-- All RLS uses the safe-wrapper helpers `app.current_tenant()` +
-- `app.is_platform()` per the migration 20260603000401 doctrine —
-- NEVER raw current_setting(...)::boolean.
--
-- `shared` schema (BRD §8.5) holds platform-wide reference data —
-- read-only for tenants, edited by SuperAdmin (write endpoint is
-- out-of-scope for this slice).

-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- inventory.products: reorder + expiry alert + product_category columns
-- ============================================================================

ALTER TABLE inventory.products
    ADD COLUMN reorder_level                int  NOT NULL DEFAULT 0
        CHECK (reorder_level >= 0);

COMMENT ON COLUMN inventory.products.reorder_level IS
    'When SUM(batches.quantity_on_hand WHERE NOT is_deleted AND NOT expired) < reorder_level, the daily ReorderScanJob emits ProductBelowReorderLevelV1. 0 disables the alert (BRD §6.5).';

ALTER TABLE inventory.products
    ADD COLUMN expiry_alert_threshold_days int  NOT NULL DEFAULT 90
        CHECK (expiry_alert_threshold_days >= 0);

COMMENT ON COLUMN inventory.products.expiry_alert_threshold_days IS
    'Batches with expiry_date <= now() + this days threshold trigger BatchExpiringSoonV1 via the daily ExpiryScanJob. Default 90 per BRD §6.5.';

ALTER TABLE inventory.products
    ADD COLUMN product_category             text NOT NULL DEFAULT 'General'
        CHECK (length(product_category) BETWEEN 1 AND 64);

COMMENT ON COLUMN inventory.products.product_category IS
    'Drives the default GST percentage via shared.product_category_gst_defaults; also matches lead ProductRanges for catalogue browsing (BRD §6.5 + Appendix C.5).';

-- Partial index supporting the ReorderScanJob's per-tenant + per-product
-- scan (live + reorder-enabled rows only).
CREATE INDEX idx_products_tenant_reorder_enabled
    ON inventory.products (tenant_id, id)
    WHERE NOT is_deleted AND reorder_level > 0;

-- ============================================================================
-- shared.product_category_gst_defaults — BRD Appendix C.5 reference data
-- ============================================================================

CREATE SCHEMA IF NOT EXISTS shared;
COMMENT ON SCHEMA shared IS
    'Platform-wide reference data (BRD §8.5). Read-only for tenants; SuperAdmin-editable via platform-tier admin endpoints.';


-- arch-test:opt-out-rls (cross-tenant reference data — BRD §8.5 declares shared.* read-only for tenants; tenant rows are not stored here)
CREATE TABLE shared.product_category_gst_defaults (
    category             text         PRIMARY KEY
        CHECK (length(category) BETWEEN 1 AND 64),
    default_gst_rate_bps int          NOT NULL
        CHECK (default_gst_rate_bps >= 0 AND default_gst_rate_bps <= 10000),
    updated_at           timestamptz  NOT NULL DEFAULT now()
);

COMMENT ON TABLE shared.product_category_gst_defaults IS
    'Default GST rate (basis points) per ProductCategory. BRD Appendix C.5 seed. Read-only for tenants; SuperAdmin write endpoint TODO.';


-- Seed per BRD Appendix C.5 (1200 bps = 12%; 1800 bps = 18%). Idempotent
-- via ON CONFLICT — re-running the migration is a no-op.
INSERT INTO shared.product_category_gst_defaults (category, default_gst_rate_bps) VALUES
    ('General',         1200),
    ('Gynaecology',     1200),
    ('Paediatrics',     1200),
    ('Diabetic',        1200),
    ('Cardiac',         1200),
    ('Ortho',           1200),
    ('Nephrology',      1200),
    ('Ayurvedic',       1200),
    ('Nutraceuticals',  1800)
ON CONFLICT (category) DO NOTHING;

-- ============================================================================
-- inventory.alert_emissions — dedup table for ExpiryScanJob + ReorderScanJob
-- ============================================================================

CREATE TABLE inventory.alert_emissions (
    tenant_id     uuid        NOT NULL REFERENCES identity.tenants(id),
    kind          text        NOT NULL
        CHECK (kind IN ('batch_expiring', 'product_below_reorder')),
    subject_id    uuid        NOT NULL,
    emitted_date  date        NOT NULL,
    emitted_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, kind, subject_id, emitted_date)
);

COMMENT ON TABLE inventory.alert_emissions IS
    'Per-day dedup ledger for ExpiryScanJob + ReorderScanJob emissions. PK ensures second-run-same-day = no-op.';

CREATE INDEX idx_alert_emissions_tenant_kind_date
    ON inventory.alert_emissions (tenant_id, kind, emitted_date DESC);

ALTER TABLE inventory.alert_emissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory.alert_emissions FORCE ROW LEVEL SECURITY;

CREATE POLICY alert_emissions_select ON inventory.alert_emissions
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY alert_emissions_insert ON inventory.alert_emissions
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- No UPDATE / DELETE policies — alert emissions are append-only.


-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS inventory.alert_emissions;
DROP TABLE IF EXISTS shared.product_category_gst_defaults;
DROP SCHEMA IF EXISTS shared;

DROP INDEX IF EXISTS inventory.idx_products_tenant_reorder_enabled;
ALTER TABLE inventory.products DROP COLUMN IF EXISTS product_category;
ALTER TABLE inventory.products DROP COLUMN IF EXISTS expiry_alert_threshold_days;
ALTER TABLE inventory.products DROP COLUMN IF EXISTS reorder_level;

-- +goose StatementEnd
