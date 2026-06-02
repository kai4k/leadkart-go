-- LeadKart Go — Phase 2 Slice 1 — CRM module init (ADR 0060).
--
-- Ships the three CRM Slice-1 aggregates + the per-module outbox table:
--
--   crm.crm_leads             → tenant-scoped lead profile + state machine,
--                               idempotency-keyed by source_purchase_id
--                               (UNIQUE NULL — same Platform lead repurchased
--                               later produces a fresh row)
--   crm.call_logs             → tenant-scoped, append-only call audit
--   crm.assignment_history    → tenant-scoped, append-only assignment audit;
--                               latest row by occurred_at IS current assignee
--                               (mirrored on crm_leads.assignee_membership_id
--                               for hot-path reads)
--   crm.outbox                → per-module outbox (mirrors identity.outbox +
--                               platform.outbox shape; ADR 0008 + 0027 + 0056)
--
-- All tables RLS+FORCE per multi-tenancy.md "FORCE ROW LEVEL SECURITY".
-- Every CRM table is tenant-scoped — no cross-tenant lead visibility ever,
-- regardless of operator status. Operators (`is_platform=true`) still get
-- read access for support / forensics per the canonical predicate
-- `tenant_id = app.current_tenant() OR app.is_platform()`.
--
-- BRD §6.3 indexed-column matrix:
--   filterable cols → btree partial indexes per common query
--   product_ranges + dosage_forms text[] → GIN (per BRD §4.3 marketplace
--                                          filters; same shape as platform_leads)
--   contact_name → pg_trgm GIN for name search (ADR 0040)
--   non-filterable supplementary fields → JSONB extra_profile
--                                          (street, gst_number, pan_number,
--                                          email, notes — NEVER in WHERE)
--
-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- crm schema
-- ============================================================================

CREATE SCHEMA IF NOT EXISTS crm;
COMMENT ON SCHEMA crm IS 'LeadKart CRM module — CrmLead, CallLog, AssignmentHistory aggregates per BRD §6.3.';

-- ============================================================================
-- crm.crm_leads
--
-- The lead profile + lifecycle. Created either:
--   (a) by the lead-purchased subscriber (source_purchase_id is set + UNIQUE),
--   (b) by manual import (slice 2+) where source_purchase_id stays NULL.
--
-- Stage state machine (per BRD §4.4 + ADR 0060):
--   new → contacted → interested → negotiation → converted (terminal)
--                                              → lost      (terminal)
--
-- Independent temperature axis: hot | warm | cold | dead.
--
-- assignee_membership_id is the CURRENT assignee (mirrored on row for hot-
-- path reads); full audit lives in crm.assignment_history.
-- ============================================================================

CREATE TABLE crm.crm_leads (
    id                       uuid        PRIMARY KEY,
    tenant_id                uuid        NOT NULL,
    source_purchase_id       uuid        NULL,      -- platform.lead-purchased.v1 PurchaseID; NULL for manual import
    source_platform_lead_id  uuid        NULL,      -- platform_leads.id; nullable for manual import

    stage                    text        NOT NULL CHECK (stage IN ('new','contacted','interested','negotiation','converted','lost')),
    temperature              text        NOT NULL CHECK (temperature IN ('hot','warm','cold','dead')),

    -- BRD §6.3 indexed columns (filterable)
    contact_name             text        NOT NULL CHECK (length(contact_name) BETWEEN 1 AND 200),
    phone_e164               text        NOT NULL CHECK (phone_e164 ~ '^\+91[0-9]{10}$'),
    city                     text        NOT NULL DEFAULT '',
    district                 text        NOT NULL DEFAULT '',
    state                    text        NOT NULL DEFAULT '',
    pincode                  text        NOT NULL DEFAULT '' CHECK (pincode = '' OR pincode ~ '^[0-9]{6}$'),
    business_type            text        NOT NULL DEFAULT '' CHECK (business_type IN ('','PCD','ThirdParty')),
    medicine_system          text        NOT NULL DEFAULT '' CHECK (medicine_system IN ('','Allopathic','Ayurvedic')),
    order_value              text        NOT NULL DEFAULT '' CHECK (order_value IN ('','Below5000','Upto25000','Upto50000','Above50000')),
    buy_timeline             text        NOT NULL DEFAULT '' CHECK (buy_timeline IN ('','WithinWeek','Within15Days','WithinMonth')),
    has_drug_licence         boolean     NOT NULL DEFAULT false,
    has_gst                  boolean     NOT NULL DEFAULT false,
    gst_verified             boolean     NOT NULL DEFAULT false,
    product_ranges           text[]      NOT NULL DEFAULT '{}',
    dosage_forms             text[]      NOT NULL DEFAULT '{}',

    -- Non-filterable supplementary data (per BRD §6.3 — NEVER in WHERE clauses)
    extra_profile            jsonb       NOT NULL DEFAULT '{}'::jsonb,

    -- Current assignee mirror (full audit in crm.assignment_history)
    assignee_membership_id   uuid        NULL,
    assigned_at              timestamptz NULL,

    -- Lifecycle metadata
    converted_at             timestamptz NULL,
    converted_by_membership_id uuid      NULL,
    lost_at                  timestamptz NULL,
    lost_by_membership_id    uuid        NULL,
    lost_reason              text        NOT NULL DEFAULT '',

    created_at               timestamptz NOT NULL,
    created_by_membership_id uuid        NULL  -- NULL for subscriber-created leads
);

-- Idempotency: a single Platform purchase produces at most one CrmLead row.
-- Partial unique (WHERE source_purchase_id IS NOT NULL) so manual imports
-- without a purchase_id can coexist freely.
CREATE UNIQUE INDEX uq_crm_leads_source_purchase
    ON crm.crm_leads (source_purchase_id)
    WHERE source_purchase_id IS NOT NULL;

-- Default list endpoint + filter-by-stage (ADR 0038 keyset on created_at, id DESC).
CREATE INDEX idx_crm_leads_tenant_stage_created
    ON crm.crm_leads (tenant_id, stage, created_at DESC, id DESC);

-- "My leads" — filter by assignee (per BRD §4.9 sales executive view).
CREATE INDEX idx_crm_leads_tenant_assignee_created
    ON crm.crm_leads (tenant_id, assignee_membership_id, created_at DESC, id DESC)
    WHERE assignee_membership_id IS NOT NULL;

-- Temperature filter (dashboard hot-list); partial excludes dead leads from
-- the most-common scan path.
CREATE INDEX idx_crm_leads_tenant_temperature
    ON crm.crm_leads (tenant_id, temperature)
    WHERE temperature != 'dead';

-- Pincode geographic filter.
CREATE INDEX idx_crm_leads_tenant_pincode
    ON crm.crm_leads (tenant_id, pincode)
    WHERE pincode != '';

-- Business-type filter (PCD vs ThirdParty).
CREATE INDEX idx_crm_leads_tenant_business_type
    ON crm.crm_leads (tenant_id, business_type)
    WHERE business_type != '';

-- text[] multi-select filters (BRD §4.3 / §6.3) — GIN.
CREATE INDEX idx_crm_leads_product_ranges_gin
    ON crm.crm_leads USING gin (product_ranges);
CREATE INDEX idx_crm_leads_dosage_forms_gin
    ON crm.crm_leads USING gin (dosage_forms);

-- contact_name trigram search (ADR 0040 — pg_trgm extension already created
-- by identity migration 20260518000001).
CREATE INDEX idx_crm_leads_contact_name_trgm
    ON crm.crm_leads USING gin (lower(contact_name) gin_trgm_ops);

ALTER TABLE crm.crm_leads ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm.crm_leads FORCE  ROW LEVEL SECURITY;

CREATE POLICY crm_leads_select ON crm.crm_leads
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_leads_insert ON crm.crm_leads
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_leads_update ON crm.crm_leads
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_leads_delete ON crm.crm_leads
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE crm.crm_leads IS
    'CrmLead aggregate per ADR 0060 + BRD §6.3. Stage state machine + independent temperature axis. source_purchase_id is the idempotency key for the lead-purchased subscriber.';

-- ============================================================================
-- crm.call_logs
--
-- Append-only call audit. Bound to a CrmLead by FK. Created via
-- POST /api/v1/crm/leads/{leadId}/calls.
-- ============================================================================

CREATE TABLE crm.call_logs (
    id                    uuid        PRIMARY KEY,
    tenant_id             uuid        NOT NULL,
    lead_id               uuid        NOT NULL REFERENCES crm.crm_leads(id) ON DELETE RESTRICT,
    outcome               text        NOT NULL CHECK (outcome IN ('connected','no_answer','busy','wrong_number','not_interested','interested','callback_requested','converted','other')),
    notes                 text        NOT NULL DEFAULT '',
    logged_by_membership_id uuid      NOT NULL,
    logged_at             timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);

-- Lead-detail view: load all calls for a lead, newest first.
CREATE INDEX idx_call_logs_tenant_lead_logged
    ON crm.call_logs (tenant_id, lead_id, logged_at DESC, id DESC);

-- "My calls today" — per-user activity report.
CREATE INDEX idx_call_logs_tenant_logger_logged
    ON crm.call_logs (tenant_id, logged_by_membership_id, logged_at DESC, id DESC);

ALTER TABLE crm.call_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm.call_logs FORCE  ROW LEVEL SECURITY;

CREATE POLICY call_logs_select ON crm.call_logs
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY call_logs_insert ON crm.call_logs
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- Append-only — no UPDATE / DELETE policies (default DENY on FORCE RLS).

COMMENT ON TABLE crm.call_logs IS
    'CallLog aggregate per ADR 0060. Append-only. Tenant-scoped FORCE RLS.';

-- ============================================================================
-- crm.assignment_history
--
-- Append-only assignment audit. Latest entry by occurred_at IS current
-- assignee (mirrored on crm_leads.assignee_membership_id for hot-path reads).
-- ============================================================================

CREATE TABLE crm.assignment_history (
    id                       uuid        PRIMARY KEY,
    tenant_id                uuid        NOT NULL,
    lead_id                  uuid        NOT NULL REFERENCES crm.crm_leads(id) ON DELETE RESTRICT,
    -- previous_assignee NULL on the first assignment for the lead.
    previous_assignee_membership_id uuid NULL,
    assignee_membership_id   uuid        NOT NULL,
    assigned_by_membership_id uuid       NOT NULL,
    reason                   text        NOT NULL DEFAULT '',
    assigned_at              timestamptz NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now()
);

-- Lead-detail view: full assignment history for a lead.
CREATE INDEX idx_assignment_history_tenant_lead_assigned
    ON crm.assignment_history (tenant_id, lead_id, assigned_at DESC, id DESC);

ALTER TABLE crm.assignment_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm.assignment_history FORCE  ROW LEVEL SECURITY;

CREATE POLICY assignment_history_select ON crm.assignment_history
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY assignment_history_insert ON crm.assignment_history
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- Append-only — no UPDATE / DELETE policies.

COMMENT ON TABLE crm.assignment_history IS
    'AssignmentHistory aggregate per ADR 0060. Append-only assignment audit. Latest row by assigned_at == current assignee (mirrored on crm.crm_leads.assignee_membership_id).';

-- NOTE: the per-module crm.outbox table was retired by ADR 0064/0067
-- (migration 20260604000002) in favour of the shared common.outbox relay
-- drained by the Watermill library Forwarder. No per-module outbox here.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS crm.assignment_history CASCADE;
DROP TABLE IF EXISTS crm.call_logs CASCADE;
DROP TABLE IF EXISTS crm.crm_leads CASCADE;
DROP SCHEMA IF EXISTS crm CASCADE;

-- +goose StatementEnd
