-- LeadKart Go — Platform marketplace: multi-buyer, tiers, dynamic pricing (ADR 0065).
--
-- Supersedes the single-buyer parts of ADR 0059. A verified lead is inventory
-- resold to several tenants up to a sale limit — not a one-off sale. This
-- migration:
--
--   * adds platform.lead_tiers   — per-tier config (default sale limit + base price)
--   * adds platform.lead_purchases — one row per (lead, buyer tenant); the
--                                    lead↔buyers many-to-many. UNIQUE(lead_id,
--                                    tenant_id) blocks a double-buy.
--   * adds platform_leads.tier + platform_leads.sale_limit (per-lead override)
--   * drops the sold_to_* / amount_paisa columns from platform_leads (a sale is
--     now a lead_purchases row, not a column flip)
--   * rewrites pl_select RLS: platform, OR you hold a purchase row, OR the lead
--     is still openly listed (purchase count < effective sale limit)
--
-- Availability = count(lead_purchases WHERE lead_id = X) < coalesce(lead.sale_limit,
-- tier.default_sale_limit). Tier eligibility (prime membership) is deferred per
-- ADR 0065 — the schema hook (tier column + lead_tiers) ships now.
--
-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- platform.lead_tiers — per-tier config
--
-- Config table (not tenant-scoped data), but it lives in the tenant-owning
-- `platform` schema so it carries RLS+FORCE + policies per multi-tenancy.md:
-- readable by anyone (tenants display tier pricing), writable platform-only.
-- ============================================================================

CREATE TABLE platform.lead_tiers (
    code               text   PRIMARY KEY CHECK (code IN ('standard','priority','premium')),
    default_sale_limit int    NOT NULL CHECK (default_sale_limit > 0),
    base_price_paisa   bigint NOT NULL CHECK (base_price_paisa >= 0),
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Seed the three tiers. Limits/prices are starting defaults (owner-tunable).
INSERT INTO platform.lead_tiers (code, default_sale_limit, base_price_paisa) VALUES
    ('standard', 6, 50000),
    ('priority', 4, 100000),
    ('premium',  2, 200000);

ALTER TABLE platform.lead_tiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.lead_tiers FORCE ROW LEVEL SECURITY;

CREATE POLICY lt_select ON platform.lead_tiers
    FOR SELECT
    USING (true);

CREATE POLICY lt_write ON platform.lead_tiers
    FOR ALL
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE platform.lead_tiers IS
    'Per-tier marketplace config: default sale limit + base price. Readable by all (tier pricing display); writes platform-only. Per ADR 0065.';

-- ============================================================================
-- platform.lead_purchases — one row per (lead, buyer tenant)
--
-- The lead↔buyers many-to-many. amount_paisa is the price THIS buyer was
-- charged (computed at purchase, snapshotted here — immutable). UNIQUE
-- (lead_id, tenant_id) makes a second purchase by the same tenant a 23505.
-- ============================================================================

CREATE TABLE platform.lead_purchases (
    id                       uuid        PRIMARY KEY,
    lead_id                  uuid        NOT NULL REFERENCES platform.platform_leads(id),
    tenant_id                uuid        NOT NULL,
    created_by_membership_id uuid        NOT NULL,  -- the buying member (row author)
    amount_paisa             bigint      NOT NULL CHECK (amount_paisa > 0),
    purchased_at             timestamptz NOT NULL,
    UNIQUE (lead_id, tenant_id)
);

-- Availability count probe: count(*) WHERE lead_id = X.
CREATE INDEX idx_lp_lead ON platform.lead_purchases (lead_id);
-- Purchaser-side "my purchased leads" keyset.
CREATE INDEX idx_lp_tenant_purchased
    ON platform.lead_purchases (tenant_id, purchased_at DESC, id DESC);

ALTER TABLE platform.lead_purchases ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.lead_purchases FORCE ROW LEVEL SECURITY;

-- A tenant sees only its own purchase rows; platform sees all.
CREATE POLICY lp_select ON platform.lead_purchases
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

-- The purchase handler runs under TxScopePlatform, so inserts are gated on
-- platform scope.
CREATE POLICY lp_write ON platform.lead_purchases
    FOR ALL
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE platform.lead_purchases IS
    'One row per (lead, buyer tenant). amount_paisa = price this buyer paid (snapshot). UNIQUE(lead_id, tenant_id) blocks a double-buy. Per ADR 0065.';

-- ============================================================================
-- platform.platform_leads — tier + sale_limit; drop the single-buyer columns
-- ============================================================================

ALTER TABLE platform.platform_leads
    ADD COLUMN tier       text NOT NULL DEFAULT 'standard'
        CHECK (tier IN ('standard','priority','premium')),
    ADD COLUMN sale_limit int  NULL
        CHECK (sale_limit IS NULL OR sale_limit > 0);

-- The old marketplace SELECT policy references sold_to_tenant_id; drop it
-- before the column (it's recreated below in count-based form).
DROP POLICY IF EXISTS pl_select ON platform.platform_leads;

-- The browse + purchaser indexes were partial on `sold_to_tenant_id`; that
-- column is going away, so drop them and rebuild as full indexes (availability
-- is now a count join, not a column predicate).
DROP INDEX IF EXISTS platform.idx_pl_unsold_keyset;
DROP INDEX IF EXISTS platform.idx_pl_state_city;
DROP INDEX IF EXISTS platform.idx_pl_district_pincode;
DROP INDEX IF EXISTS platform.idx_pl_business;
DROP INDEX IF EXISTS platform.idx_pl_compliance;
DROP INDEX IF EXISTS platform.idx_pl_product_ranges_gin;
DROP INDEX IF EXISTS platform.idx_pl_dosage_forms_gin;
DROP INDEX IF EXISTS platform.idx_pl_sold_to_tenant;

ALTER TABLE platform.platform_leads
    DROP COLUMN sold_to_tenant_id,
    DROP COLUMN sold_at,
    DROP COLUMN sold_to_membership_id,
    DROP COLUMN amount_paisa;

-- Rebuilt full browse indexes (BRD §4.3 filters).
CREATE INDEX idx_pl_verified_keyset
    ON platform.platform_leads (verified_at DESC, id DESC);
CREATE INDEX idx_pl_state_city
    ON platform.platform_leads (state_geo, city);
CREATE INDEX idx_pl_district_pincode
    ON platform.platform_leads (district, pincode);
CREATE INDEX idx_pl_business
    ON platform.platform_leads (business_type, medicine_system, order_value, buy_timeline);
CREATE INDEX idx_pl_compliance
    ON platform.platform_leads (has_drug_licence, has_gst, gst_verified);
CREATE INDEX idx_pl_product_ranges_gin
    ON platform.platform_leads USING gin (product_ranges);
CREATE INDEX idx_pl_dosage_forms_gin
    ON platform.platform_leads USING gin (dosage_forms);
CREATE INDEX idx_pl_tier ON platform.platform_leads (tier);

-- Rewrite the marketplace SELECT policy: platform sees all; a tenant sees a
-- lead if it holds a purchase row OR the lead is still openly listed (purchase
-- count below the effective sale limit). Tier eligibility is a query concern.
-- (The old policy was dropped above, before its sold_to_tenant_id column.)
CREATE POLICY pl_select ON platform.platform_leads
    FOR SELECT
    USING (
        app.is_platform()
        OR EXISTS (
            SELECT 1 FROM platform.lead_purchases lp
            WHERE lp.lead_id = platform_leads.id
              AND lp.tenant_id = app.current_tenant()
        )
        OR (
            SELECT count(*) FROM platform.lead_purchases lp2
            WHERE lp2.lead_id = platform_leads.id
        ) < COALESCE(
            platform_leads.sale_limit,
            (SELECT t.default_sale_limit FROM platform.lead_tiers t WHERE t.code = platform_leads.tier)
        )
    );

COMMENT ON TABLE platform.platform_leads IS
    'Verified leads in the marketplace, resold to multiple tenants up to a sale limit (ADR 0065). RLS: platform, or you hold a lead_purchases row, or the lead is still openly listed (purchases < effective sale limit).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the single-buyer shape (best-effort; loses multi-buyer rows).
DROP POLICY IF EXISTS pl_select ON platform.platform_leads;

DROP INDEX IF EXISTS platform.idx_pl_verified_keyset;
DROP INDEX IF EXISTS platform.idx_pl_state_city;
DROP INDEX IF EXISTS platform.idx_pl_district_pincode;
DROP INDEX IF EXISTS platform.idx_pl_business;
DROP INDEX IF EXISTS platform.idx_pl_compliance;
DROP INDEX IF EXISTS platform.idx_pl_product_ranges_gin;
DROP INDEX IF EXISTS platform.idx_pl_dosage_forms_gin;
DROP INDEX IF EXISTS platform.idx_pl_tier;

ALTER TABLE platform.platform_leads
    ADD COLUMN sold_to_tenant_id     uuid        NULL,
    ADD COLUMN sold_at               timestamptz NULL,
    ADD COLUMN sold_to_membership_id uuid        NULL,
    ADD COLUMN amount_paisa          bigint      NOT NULL DEFAULT 0;

ALTER TABLE platform.platform_leads
    DROP COLUMN tier,
    DROP COLUMN sale_limit;

CREATE INDEX idx_pl_unsold_keyset
    ON platform.platform_leads (verified_at DESC, id DESC)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_state_city
    ON platform.platform_leads (state_geo, city)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_district_pincode
    ON platform.platform_leads (district, pincode)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_business
    ON platform.platform_leads (business_type, medicine_system, order_value, buy_timeline)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_compliance
    ON platform.platform_leads (has_drug_licence, has_gst, gst_verified)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_product_ranges_gin
    ON platform.platform_leads USING gin (product_ranges)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_dosage_forms_gin
    ON platform.platform_leads USING gin (dosage_forms)
    WHERE sold_to_tenant_id IS NULL;
CREATE INDEX idx_pl_sold_to_tenant
    ON platform.platform_leads (sold_to_tenant_id, sold_at DESC)
    WHERE sold_to_tenant_id IS NOT NULL;

CREATE POLICY pl_select ON platform.platform_leads
    FOR SELECT
    USING (
        sold_to_tenant_id IS NULL
        OR sold_to_tenant_id = app.current_tenant()
        OR app.is_platform()
    );

DROP TABLE IF EXISTS platform.lead_purchases CASCADE;
DROP TABLE IF EXISTS platform.lead_tiers CASCADE;

-- +goose StatementEnd
