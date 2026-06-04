-- LeadKart Go — Phase 2 Slice A.2 — CRM Reminder aggregate.
--
-- Ships the crm.reminders table per BRD §4.5 / §4.6 / §4.7:
--
--   - BRD §4.5 Callback Window  — when a caller logs a callback window,
--                                 the CallLogged subscriber auto-creates
--                                 a callback reminder.
--   - BRD §4.6 Reminders        — the dashboard notification surface.
--                                 today / upcoming / overdue rendered
--                                 from the pending partial index below.
--   - BRD §4.7 Mature-lead rule — a daily river job creates a
--                                 mature-lead reminder for converted
--                                 leads with no reorder activity within
--                                 the configured window (3 months at
--                                 v0.2; v0.3 wires actual reorder
--                                 tracking, the v0.2 approximation uses
--                                 crm.crm_leads.converted_at < cutoff).
--
-- Aggregate boundary (per ADR 0060): Reminder is its own aggregate.
-- Lifecycle (pending → sent | cancelled) lives on the row; the parent
-- CrmLead is referenced by composite FK ONLY — no navigation property.
--
-- RLS+FORCE per multi-tenancy.md "FORCE ROW LEVEL SECURITY" — same
-- shape as every other CRM table. Policies use the app.current_tenant()
-- / app.is_platform() safe wrappers (per
-- 20260603000401_role_hierarchy_edges_rls_safe_wrapper.sql) — never the
-- raw current_setting()::boolean cast.
--
-- Idempotency partial unique indexes (the load-bearing part of this
-- table — drives the at-most-once subscriber + at-most-one-pending-
-- per-lead cron guards):
--
--   uq_crm_reminders_callback_pending — (tenant_id, source_call_log_id)
--                                       WHERE type='callback' AND
--                                             state='pending'
--   uq_crm_reminders_mature_pending   — (tenant_id, lead_id)
--                                       WHERE type='mature_lead' AND
--                                             state='pending'
--
-- audit_chain columns: created_by_membership_id is part of the
-- aggregate (NULL for subscriber / cron-created reminders). No further
-- audit_chain columns needed beyond what the aggregate itself carries —
-- assignment-history-like audit tables are not in scope for v0.2
-- reminders.

-- +goose Up
-- +goose StatementBegin

-- Composite UNIQUE on the parent table — required BEFORE crm.reminders'
-- FOREIGN KEY (lead_id, tenant_id) can reference crm.crm_leads (id, tenant_id)
-- (Postgres needs a matching unique constraint on the referenced columns).
ALTER TABLE crm.crm_leads
    ADD CONSTRAINT uq_crm_leads_id_tenant UNIQUE (id, tenant_id);

CREATE TABLE crm.reminders (
    id                          uuid        PRIMARY KEY,
    tenant_id                   uuid        NOT NULL,
    -- Composite FK to crm.crm_leads — the (id, tenant_id) shape blocks
    -- cross-tenant lead references at the DB layer (defense-in-depth
    -- behind RLS).
    lead_id                     uuid        NOT NULL,
    assigned_to_membership_id   uuid        NOT NULL,
    created_by_membership_id    uuid        NULL,   -- NULL for subscriber / cron created
    source_call_log_id          uuid        NULL,   -- populated only for type='callback'

    type                        text        NOT NULL CHECK (type IN ('callback','mature_lead','manual')),
    state                       text        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending','sent','cancelled')),

    due_at                      timestamptz NOT NULL,
    notes                       text        NOT NULL DEFAULT ''
                                CHECK (length(notes) <= 2000),

    -- Terminal-state metadata.
    sent_at                     timestamptz NULL,
    marked_sent_by_membership_id uuid       NULL,
    cancelled_at                timestamptz NULL,
    cancelled_by_membership_id  uuid        NULL,
    cancel_reason               text        NOT NULL DEFAULT ''
                                CHECK (length(cancel_reason) <= 1000),

    created_at                  timestamptz NOT NULL,

    -- Composite FK to crm.crm_leads (id, tenant_id) — requires a
    -- matching unique constraint on the parent. crm.crm_leads PRIMARY
    -- KEY is (id) alone; we add a UNIQUE (id, tenant_id) deferred
    -- constraint below to satisfy this FK shape (mirror of platform.lead
    -- FK pattern). RESTRICT preserves the lead through reminder rows.
    CONSTRAINT fk_crm_reminders_lead
        FOREIGN KEY (lead_id, tenant_id)
        REFERENCES crm.crm_leads (id, tenant_id)
        ON DELETE RESTRICT,

    -- Callback reminders MUST carry a source_call_log_id; non-callback
    -- types MUST NOT (the partial unique index assumes this).
    CONSTRAINT chk_crm_reminders_callback_source CHECK (
        (type = 'callback' AND source_call_log_id IS NOT NULL) OR
        (type <> 'callback' AND source_call_log_id IS NULL)
    ),

    -- Terminal-state metadata coherence — when state='sent' the
    -- sent_at + marked_sent_by_membership_id MUST be set; when
    -- state='cancelled' the cancelled_at + cancelled_by_membership_id
    -- + cancel_reason MUST be set.
    CONSTRAINT chk_crm_reminders_state_coherent CHECK (
        (state <> 'sent' OR (sent_at IS NOT NULL AND marked_sent_by_membership_id IS NOT NULL))
        AND
        (state <> 'cancelled' OR (cancelled_at IS NOT NULL AND cancelled_by_membership_id IS NOT NULL AND length(cancel_reason) > 0))
    )
);

-- Dashboard hot-path: "today / upcoming / overdue" per assignee, sorted
-- by due_at ASC. Partial index excludes sent + cancelled rows so the
-- index stays small.
CREATE INDEX idx_crm_reminders_pending_assignee_due
    ON crm.reminders (tenant_id, assigned_to_membership_id, due_at, id)
    WHERE state = 'pending';

-- Lead-detail panel: "reminders for this lead" (all states).
CREATE INDEX idx_crm_reminders_tenant_lead_due
    ON crm.reminders (tenant_id, lead_id, due_at DESC, id DESC);

-- Partial unique gate for the CallLogged subscriber's at-most-once
-- guarantee per the slice brief. A duplicate broker delivery for the
-- same call_log_id finds the existing pending callback reminder + the
-- subscriber treats SQLSTATE 23505 as success (ACK).
CREATE UNIQUE INDEX uq_crm_reminders_callback_pending
    ON crm.reminders (tenant_id, source_call_log_id)
    WHERE type = 'callback' AND state = 'pending';

-- Partial unique gate for the mature-lead scheduler: at most one
-- pending mature-lead reminder per (tenant, lead).
CREATE UNIQUE INDEX uq_crm_reminders_mature_pending
    ON crm.reminders (tenant_id, lead_id)
    WHERE type = 'mature_lead' AND state = 'pending';

ALTER TABLE crm.reminders ENABLE ROW LEVEL SECURITY;
ALTER TABLE crm.reminders FORCE  ROW LEVEL SECURITY;

CREATE POLICY crm_reminders_select ON crm.reminders
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_reminders_insert ON crm.reminders
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_reminders_update ON crm.reminders
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY crm_reminders_delete ON crm.reminders
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE crm.reminders IS
    'Reminder aggregate per BRD §4.5/§4.6/§4.7 + slice A.2. Notification surface — pending → sent | cancelled state machine. Auto-created by the CallLogged subscriber (callback type) + mature-lead daily scan (mature_lead type); manual type via HTTP.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE crm.crm_leads DROP CONSTRAINT IF EXISTS uq_crm_leads_id_tenant;
DROP TABLE IF EXISTS crm.reminders CASCADE;

-- +goose StatementEnd
