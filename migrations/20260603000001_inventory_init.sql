-- LeadKart Go — Inventory module initial schema (Slice 1).
-- Per BRD §6.5 + ADR 0001 (modular monolith) + ADR 0006 (RLS) +
-- ADR 0038 (cursor pagination) + ADR 0040 (pg_trgm search).
--
-- Module: Inventory. Owns: Product, Batch, StockMovement.
-- All three aggregates are TENANT-SCOPED — RLS + FORCE per LeadKart canon.
--
-- Tenant-scoping verdicts (rationale per multi-tenancy.md):
--   inventory.products         tenant-scoped, RLS + FORCE
--   inventory.batches          tenant-scoped, RLS + FORCE
--                              + composite-FK to products(id, tenant_id)
--                              + explicit `version` concurrency token
--                              (Vernon IDDD ch.10; EF Core canon)
--   inventory.stock_movements  tenant-scoped, RLS + FORCE
--                              + composite-FK to batches(id, tenant_id)
--                              append-only ledger; no UPDATE/DELETE policies
--
-- Money: int64 paise (Stripe canon; never float). Columns are `bigint`.
-- Dates (manufacture_date, expiry_date): SQL `date` type — stored as
-- Postgres date, mapped via pgtype.Date in adapter only.
--
-- All CREATE INDEX uses default (in-tx) form — goose wraps in tx, same
-- early-phase pattern as 20260518000001 (no CONCURRENTLY).

-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS inventory;
COMMENT ON SCHEMA inventory IS 'LeadKart Inventory module — Products, Batches, StockMovements (BRD §6.5).';

-- ============================================================================
-- inventory.products
-- Pharma product master per BRD §6.5. Tenant-scoped (each tenant maintains
-- its own catalog). SKU uniqueness is per-tenant on LIVE rows only — soft-
-- deleted rows can collide so admins can recreate after cleanup.
-- ============================================================================

CREATE TABLE inventory.products (
    id                 uuid        PRIMARY KEY,
    tenant_id          uuid        NOT NULL REFERENCES identity.tenants(id),
    sku                text        NOT NULL CHECK (length(sku) BETWEEN 1 AND 64),
    name               text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    dosage_form        text        NOT NULL CHECK (length(dosage_form) BETWEEN 1 AND 50),
    pack_size          text        NOT NULL CHECK (length(pack_size) BETWEEN 1 AND 100),
    hsn_code           text        NOT NULL CHECK (length(hsn_code) BETWEEN 4 AND 10),
    gst_rate_bps       integer     NOT NULL CHECK (gst_rate_bps BETWEEN 0 AND 10000),
    manufacturer       text        NOT NULL DEFAULT '' CHECK (length(manufacturer) <= 200),
    is_active          boolean     NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    is_deleted         boolean     NOT NULL DEFAULT false,
    deleted_at         timestamptz NULL,
    deleted_by         text        NULL,
    -- (id, tenant_id) candidate key — referenced by batches composite-FK
    -- below to enforce no cross-tenant batch-to-product linking (the
    -- documented anti-mix-up pattern from database.md).
    CONSTRAINT uq_products_id_tenant UNIQUE (id, tenant_id)
);

-- Per-tenant SKU uniqueness on LIVE rows only (spec-required partial index).
CREATE UNIQUE INDEX uq_products_tenant_sku_live
    ON inventory.products (tenant_id, sku) WHERE NOT is_deleted;

-- Composite keyset for cursor pagination on (created_at, id) DESC under
-- tenant filter — matches ADR 0038 query shape (live-only listing).
CREATE INDEX idx_products_tenant_created_keyset
    ON inventory.products (tenant_id, created_at DESC, id DESC)
    WHERE NOT is_deleted;

-- Partial-index sweep: list-by-active for the common UI default filter.
CREATE INDEX idx_products_tenant_active
    ON inventory.products (tenant_id, created_at DESC, id DESC)
    WHERE NOT is_deleted AND is_active;

-- pg_trgm search per ADR 0040 — combined SKU + name lower-cased expression.
-- Skip btree_gin (deferred per ADR 0040) — planner combines this GIN with
-- the existing tenant-id btree via Bitmap And; same posture as identity.
CREATE INDEX idx_products_search_trgm
    ON inventory.products
    USING gin ((lower(sku) || ' ' || lower(name)) gin_trgm_ops)
    WHERE NOT is_deleted;

ALTER TABLE inventory.products ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory.products FORCE  ROW LEVEL SECURITY;

CREATE POLICY products_select ON inventory.products
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY products_insert ON inventory.products
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY products_modify ON inventory.products
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY products_delete ON inventory.products
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE inventory.products IS
    'Product aggregate. Tenant-scoped, FORCE RLS. SKU unique per tenant on live rows (partial index).';

-- ============================================================================
-- inventory.batches
-- Per-product manufacturing batch. Child of products aggregate (referenced
-- by ID per BRD §6.5 + DDD aggregate-root rule). Holds running stock-on-hand
-- + explicit concurrency token (`version`) for high-contention stock writes.
--
-- Composite-FK on (product_id, tenant_id) → products(id, tenant_id)
-- enforces no cross-tenant batch-to-product linkage (anti-mix-up pattern).
-- ============================================================================

CREATE TABLE inventory.batches (
    id                              uuid        PRIMARY KEY,
    product_id                      uuid        NOT NULL,
    tenant_id                       uuid        NOT NULL,
    batch_number                    text        NOT NULL CHECK (length(batch_number) BETWEEN 1 AND 100),
    manufacture_date                date        NOT NULL,
    expiry_date                     date        NOT NULL,
    manufacturer_name               text        NOT NULL CHECK (length(manufacturer_name) BETWEEN 1 AND 200),
    manufacturing_licence_number    text        NOT NULL CHECK (length(manufacturing_licence_number) BETWEEN 1 AND 100),
    mrp_paise                       bigint      NOT NULL CHECK (mrp_paise >= 0),
    purchase_price_paise            bigint      NOT NULL CHECK (purchase_price_paise >= 0),
    quantity_on_hand                bigint      NOT NULL DEFAULT 0 CHECK (quantity_on_hand >= 0),
    -- Explicit concurrency token per Vernon IDDD ch.10 + EF Core canon.
    -- Incremented on every UPDATE; the adapter writes
    -- `WHERE version = $current` and treats rows-affected=0 as a conflict.
    -- Spec calls this "xmin pattern"; explicit version is the canonical
    -- portable variant (xmin is opaque + recycled at vacuum).
    version                         bigint      NOT NULL DEFAULT 0,
    created_at                      timestamptz NOT NULL,
    updated_at                      timestamptz NOT NULL,
    is_deleted                      boolean     NOT NULL DEFAULT false,
    deleted_at                      timestamptz NULL,
    deleted_by                      text        NULL,
    CONSTRAINT chk_batch_expiry_after_manufacture
        CHECK (expiry_date > manufacture_date),
    -- Composite FK ensures the batch's tenant_id matches the product's
    -- tenant_id — same anti-mix-up pattern as identity.role_assignments.
    CONSTRAINT fk_batches_product_same_tenant
        FOREIGN KEY (product_id, tenant_id)
            REFERENCES inventory.products(id, tenant_id),
    -- Candidate key for stock_movements composite-FK.
    CONSTRAINT uq_batches_id_tenant UNIQUE (id, tenant_id)
);

-- Lookup-by-product (frequent: "list product's batches" view).
CREATE INDEX idx_batches_product
    ON inventory.batches (product_id, expiry_date ASC, id DESC)
    WHERE NOT is_deleted;

-- Tenant-wide expiry sweep (future "expiring in 90 days" alert query).
CREATE INDEX idx_batches_tenant_expiry
    ON inventory.batches (tenant_id, expiry_date ASC)
    WHERE NOT is_deleted;

-- Per-product uniqueness of batch_number on LIVE rows.
CREATE UNIQUE INDEX uq_batches_product_number_live
    ON inventory.batches (product_id, batch_number) WHERE NOT is_deleted;

ALTER TABLE inventory.batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory.batches FORCE  ROW LEVEL SECURITY;

CREATE POLICY batches_select ON inventory.batches
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY batches_insert ON inventory.batches
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY batches_modify ON inventory.batches
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY batches_delete ON inventory.batches
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE inventory.batches IS
    'Batch aggregate. Tenant-scoped, FORCE RLS. Composite-FK to products enforces same-tenant linkage. `version` column drives optimistic-concurrency retry.';

-- ============================================================================
-- inventory.stock_movements
-- Append-only ledger of stock changes per batch. Own aggregate (Vernon IDDD
-- ch.7 — append-only ledgers warrant a separate aggregate lifecycle from the
-- thing they observe). NO UPDATE/DELETE policies — by design.
--
-- type: 'inbound' | 'outbound' | 'adjustment' | 'reservation' | 'release'
-- quantity: SIGNED bigint. Outbound/Adjustment-down carry negative values.
--           Reservation/Release are non-mutating to quantity_on_hand but
--           recorded for audit trail.
-- source_reference: nullable opaque string (future PurchaseOrderID,
--                   OrderID, manual-adjustment ticket id).
-- ============================================================================

CREATE TABLE inventory.stock_movements (
    id                       uuid        PRIMARY KEY,
    batch_id                 uuid        NOT NULL,
    product_id               uuid        NOT NULL,
    tenant_id                uuid        NOT NULL,
    type                     text        NOT NULL CHECK (type IN ('inbound','outbound','adjustment','reservation','release')),
    quantity                 bigint      NOT NULL,
    quantity_on_hand_after   bigint      NOT NULL CHECK (quantity_on_hand_after >= 0),
    reason                   text        NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    actor_membership_id      uuid        NOT NULL,
    source_reference         text        NULL CHECK (source_reference IS NULL OR length(source_reference) <= 200),
    occurred_at              timestamptz NOT NULL,
    -- Composite FK guarantees the movement's tenant_id matches the batch's
    -- tenant_id. Same anti-mix-up pattern as role_assignments.
    CONSTRAINT fk_movements_batch_same_tenant
        FOREIGN KEY (batch_id, tenant_id)
            REFERENCES inventory.batches(id, tenant_id)
);

-- Spec-required composite keyset for cursor pagination on the per-batch
-- ledger (movements?cursor=&page_size=) ordered (occurred_at, id) DESC.
CREATE INDEX idx_movements_batch_keyset
    ON inventory.stock_movements (batch_id, occurred_at DESC, id DESC);

-- Tenant-wide secondary index for cross-product ledger views.
CREATE INDEX idx_movements_tenant_occurred
    ON inventory.stock_movements (tenant_id, occurred_at DESC, id DESC);

-- Filter-by-type for the ?type= ledger filter on the route.
CREATE INDEX idx_movements_batch_type_keyset
    ON inventory.stock_movements (batch_id, type, occurred_at DESC, id DESC);

ALTER TABLE inventory.stock_movements ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory.stock_movements FORCE  ROW LEVEL SECURITY;

CREATE POLICY movements_select ON inventory.stock_movements
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY movements_insert ON inventory.stock_movements
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- Append-only ledger — NO UPDATE policy, NO DELETE policy.
-- (Postgres default-deny when RLS is enabled + no policy matches the
-- command. Combined with the platform-tier nuke path documented in
-- data-retention.md, that's enough.)

COMMENT ON TABLE inventory.stock_movements IS
    'Append-only stock-movement ledger. Tenant-scoped, FORCE RLS. NO update/delete policies (Vernon IDDD append-only aggregate).';

-- NOTE: the per-module inventory.outbox table was retired by ADR 0064/0067
-- (migration 20260604000002) in favour of the shared common.outbox relay
-- drained by the Watermill library Forwarder. No per-module outbox here.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS inventory.stock_movements CASCADE;
DROP TABLE IF EXISTS inventory.batches CASCADE;
DROP TABLE IF EXISTS inventory.products CASCADE;
DROP SCHEMA IF EXISTS inventory CASCADE;

-- +goose StatementEnd
