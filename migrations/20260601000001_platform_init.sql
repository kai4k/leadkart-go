-- LeadKart Go — Phase 2 Slice 1 — Platform module init (ADR 0059).
--
-- Ships the four Platform aggregates + the per-module outbox table:
--
--   platform.unverified_contacts   → Platform-only (no tenant scope),
--                                    Lead Agent work queue
--   platform.verification_calls    → Platform-only, append-only call log
--   platform.platform_leads        → Platform write; marketplace cross-tenant
--                                    SELECT for unsold rows (ADR 0059 RLS
--                                    exception)
--   platform.lead_credits          → tenant-scoped, optimistic concurrency
--                                    via explicit `version` column
--   platform.outbox                → per-module outbox (mirrors
--                                    identity.outbox shape; ADR 0008 + 0027)
--
-- All tables RLS+FORCE per multi-tenancy.md "FORCE ROW LEVEL SECURITY".
-- Platform-only tables use `app.is_platform()` as the predicate; tenant-
-- scoped tables use `tenant_id = app.current_tenant() OR app.is_platform()`.
--
-- BRD §5 lead form fields live on platform_leads + unverified_contacts as
-- typed columns; ProductRanges + DosageForms are text[] with GIN indexes
-- per BRD §4.3 (marketplace filters).
--
-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- platform schema
-- ============================================================================

CREATE SCHEMA IF NOT EXISTS platform;

-- ============================================================================
-- platform.unverified_contacts
--
-- Lead Agent's work queue. Created by POST /platform/unverified-contacts;
-- transitions to Verified | Rejected | Busy via POST .../verify | .../reject |
-- .../calls (when outcome=Busy). On Verified, the command handler creates a
-- platform_leads row in the SAME tx.
--
-- All BRD §5 fields captured at creation (frontend pre-fills city/district/
-- state from pincode via shared.pincodes lookup at the BFF layer).
--
-- Platform-only: no tenant_id column.
-- ============================================================================

CREATE TABLE platform.unverified_contacts (
    id                   uuid        PRIMARY KEY,
    state                text        NOT NULL CHECK (state IN ('new','in_call','verified','rejected','busy')),
    rejection_reason     text        NOT NULL DEFAULT '',
    busy_callback_at     timestamptz NULL,        -- next-call window start
    busy_callback_end_at timestamptz NULL,        -- next-call window end
    platform_lead_id     uuid        NULL,        -- backfilled on Verified transition

    -- BRD §5 lead form fields (locked)
    contact_name         text        NOT NULL CHECK (length(contact_name) BETWEEN 2 AND 200),
    mobile_e164          text        NOT NULL CHECK (mobile_e164 ~ '^\+91[0-9]{10}$'),
    email                text        NOT NULL DEFAULT '',
    pincode              text        NOT NULL CHECK (pincode ~ '^[0-9]{6}$'),
    city                 text        NOT NULL,
    district             text        NOT NULL,
    state_geo            text        NOT NULL,
    street               text        NOT NULL DEFAULT '',
    has_drug_licence     boolean     NOT NULL,
    has_gst              boolean     NOT NULL,
    gst_number           text        NOT NULL DEFAULT '',
    has_pan              boolean     NOT NULL,
    pan_number           text        NOT NULL DEFAULT '',
    business_type        text        NOT NULL CHECK (business_type IN ('PCD','ThirdParty')),
    medicine_system      text        NOT NULL CHECK (medicine_system IN ('Allopathic','Ayurvedic')),
    product_ranges       text[]      NOT NULL DEFAULT '{}',
    dosage_forms         text[]      NOT NULL DEFAULT '{}',
    order_value          text        NOT NULL CHECK (order_value IN ('Below5000','Upto25000','Upto50000','Above50000')),
    buy_timeline         text        NOT NULL CHECK (buy_timeline IN ('WithinWeek','Within15Days','WithinMonth')),

    created_at           timestamptz NOT NULL,
    created_by_membership_id uuid    NOT NULL,
    verified_at          timestamptz NULL,
    verified_by_membership_id uuid   NULL,
    rejected_at          timestamptz NULL,
    rejected_by_membership_id uuid   NULL
);

-- Keyset pagination on (created_at, id) DESC — Lead Agent dashboard
-- "show me my queue" + Platform-operator audit.
CREATE INDEX idx_uvc_state_created_keyset
    ON platform.unverified_contacts (state, created_at DESC, id DESC);

-- Mobile lookup — operator dedup probe ("did we already source this
-- number?"). Not unique because the same number can legitimately appear
-- across rejected + new tries.
CREATE INDEX idx_uvc_mobile ON platform.unverified_contacts (mobile_e164);

ALTER TABLE platform.unverified_contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.unverified_contacts FORCE  ROW LEVEL SECURITY;

CREATE POLICY uvc_platform_only ON platform.unverified_contacts
    FOR ALL
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE platform.unverified_contacts IS
    'Lead Agent work queue. Platform-only (no tenant). State machine: new → in_call → verified | rejected | busy. On verified, a platform_leads row is created in the same tx.';

-- ============================================================================
-- platform.verification_calls
--
-- Append-only call log. One row per outbound call attempt. Outcome enum +
-- optional callback window (only populated when outcome=busy).
--
-- Platform-only: no tenant_id column.
-- ============================================================================

CREATE TABLE platform.verification_calls (
    id                       uuid        PRIMARY KEY,
    contact_id               uuid        NOT NULL REFERENCES platform.unverified_contacts(id),
    outcome_code             text        NOT NULL CHECK (outcome_code IN ('verified','rejected','busy','no_answer','wrong_number')),
    notes                    text        NOT NULL DEFAULT '',
    callback_window_start_at timestamptz NULL,
    callback_window_end_at   timestamptz NULL,
    logged_at                timestamptz NOT NULL,
    logged_by_membership_id  uuid        NOT NULL
);

CREATE INDEX idx_vc_contact_logged
    ON platform.verification_calls (contact_id, logged_at DESC);

ALTER TABLE platform.verification_calls ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.verification_calls FORCE  ROW LEVEL SECURITY;

CREATE POLICY vc_platform_only ON platform.verification_calls
    FOR ALL
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE platform.verification_calls IS
    'Append-only call log. Each row immutable post-insert. Callback window populated when outcome=busy (auto-creates a Reminder downstream in Phase 2.2).';

-- ============================================================================
-- platform.platform_leads
--
-- Marketplace listing. Created from a Verified UnverifiedContact. Once
-- sold (sold_to_tenant_id IS NOT NULL), removed from browse but retained
-- for the purchasing tenant + platform audit.
--
-- RLS posture per ADR 0059: SELECT open to (unsold OR your-own-purchased
-- OR platform); ALL writes platform-only.
-- ============================================================================

CREATE TABLE platform.platform_leads (
    id                  uuid        PRIMARY KEY,
    source_contact_id   uuid        NOT NULL REFERENCES platform.unverified_contacts(id),
    sold_to_tenant_id   uuid        NULL,            -- NULL = available in marketplace
    sold_at             timestamptz NULL,
    sold_to_membership_id uuid      NULL,            -- which tenant member purchased
    amount_paisa        bigint      NOT NULL DEFAULT 0,  -- price paid in INR paise

    -- BRD §5 lead form fields (snapshotted at verification — never edited)
    contact_name        text        NOT NULL,
    mobile_e164         text        NOT NULL,
    email               text        NOT NULL DEFAULT '',
    pincode             text        NOT NULL,
    city                text        NOT NULL,
    district            text        NOT NULL,
    state_geo           text        NOT NULL,
    street              text        NOT NULL DEFAULT '',
    has_drug_licence    boolean     NOT NULL,
    has_gst             boolean     NOT NULL,
    gst_number          text        NOT NULL DEFAULT '',
    gst_verified        boolean     NOT NULL DEFAULT false,
    has_pan             boolean     NOT NULL,
    pan_number          text        NOT NULL DEFAULT '',
    business_type       text        NOT NULL,
    medicine_system     text        NOT NULL,
    product_ranges      text[]      NOT NULL DEFAULT '{}',
    dosage_forms        text[]      NOT NULL DEFAULT '{}',
    order_value         text        NOT NULL,
    buy_timeline        text        NOT NULL,

    verified_at         timestamptz NOT NULL,
    verified_by_membership_id uuid  NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Marketplace browse indexes per BRD §4.3 filters.
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
-- Purchaser-side lookup — "show me my purchased leads".
CREATE INDEX idx_pl_sold_to_tenant
    ON platform.platform_leads (sold_to_tenant_id, sold_at DESC)
    WHERE sold_to_tenant_id IS NOT NULL;

ALTER TABLE platform.platform_leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.platform_leads FORCE  ROW LEVEL SECURITY;

-- Marketplace SELECT: unsold rows visible to everyone; sold rows visible
-- only to the purchaser + platform. ADR 0059 exception.
CREATE POLICY pl_select ON platform.platform_leads
    FOR SELECT
    USING (
        sold_to_tenant_id IS NULL
        OR sold_to_tenant_id = app.current_tenant()
        OR app.is_platform()
    );

-- All writes (INSERT from verify, UPDATE from purchase) gated on platform
-- scope. Purchase handler runs under TxScopePlatform for the UPDATE.
CREATE POLICY pl_write_platform ON platform.platform_leads
    FOR ALL
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE platform.platform_leads IS
    'Verified leads in the marketplace. RLS: unsold rows openly browsable; sold rows visible only to purchaser + platform. Per ADR 0059.';

-- ============================================================================
-- platform.lead_credits
--
-- Per-tenant credit balance. Optimistic concurrency via explicit `version`
-- column (ADR 0059 — mirror of .NET ADR-015 xmin pattern, sqlc-friendly
-- shape).
-- ============================================================================

CREATE TABLE platform.lead_credits (
    tenant_id  uuid        PRIMARY KEY,
    balance    bigint      NOT NULL DEFAULT 0 CHECK (balance >= 0),
    version    bigint      NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

ALTER TABLE platform.lead_credits ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.lead_credits FORCE  ROW LEVEL SECURITY;

CREATE POLICY lc_select ON platform.lead_credits
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY lc_write ON platform.lead_credits
    FOR ALL
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE platform.lead_credits IS
    'Per-tenant credit balance. Optimistic concurrency: every UPDATE checks WHERE version = $old_version + sets version = $old_version + 1; 0 rows affected → conflict → command handler retries with backoff. Per ADR 0059.';

-- NOTE: the per-module platform.outbox table was retired by ADR 0064/0067
-- (migration 20260604000002) in favour of the shared common.outbox relay
-- drained by the Watermill library Forwarder. No per-module outbox here.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS platform.lead_credits CASCADE;
DROP TABLE IF EXISTS platform.platform_leads CASCADE;
DROP TABLE IF EXISTS platform.verification_calls CASCADE;
DROP TABLE IF EXISTS platform.unverified_contacts CASCADE;
DROP SCHEMA IF EXISTS platform CASCADE;

-- +goose StatementEnd
