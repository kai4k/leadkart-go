-- LeadKart Go — messaging + audit infrastructure tables for v0.2.
--
-- Three tables, three concerns:
--
--  1. app.command_idempotency        — HTTP X-Command-Id replay store
--                                       (Stripe Idempotency-Key canon).
--  2. buildingblocks.audit_log_entry — auto-write per command via the
--                                       Watermill router AuditLoggingMiddleware
--                                       (.NET messaging.md "Audit log middleware").
--  3. identity.processed_messages    — per-handler inbox dedup for the
--                                       IdempotentReceiver wrapper
--                                       (messaging.md "Idempotency Layer 2").
--
-- All three tables non-RLS — they're cross-cutting infrastructure, not
-- tenanted aggregate state. Reads happen under platform scope.

-- +goose Up
-- +goose StatementBegin

-- ===========================================================================
-- app.command_idempotency
-- HTTP X-Command-Id idempotency middleware store.
--
-- Lifecycle:
--   1. First arrival of an X-Command-Id: handler runs; response captured
--      (status + body bytea + body_hash); row inserted with TTL.
--   2. Replay with same X-Command-Id + matching body_hash: stored
--      response returned with X-Idempotent-Replay: true (no handler call).
--   3. Replay with same X-Command-Id + DIFFERENT body_hash: HTTP 422
--      (key reuse with mismatched body — security signal).
--   4. After expires_at: row purged by background job (river); subsequent
--      use of same key starts fresh.
--
-- Citation: Stripe API docs "Idempotency-Key" (canonical pattern);
-- GitHub API request-id semantics; RFC 9110 §17 idempotent methods.
-- ===========================================================================

CREATE TABLE app.command_idempotency (
    command_id      text        PRIMARY KEY CHECK (length(command_id) > 0 AND length(command_id) <= 200),
    body_hash       text        NOT NULL,
    response_status integer     NOT NULL,
    response_body   bytea       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL
);

CREATE INDEX idx_idempotency_expires ON app.command_idempotency (expires_at);

COMMENT ON TABLE app.command_idempotency IS
    'X-Command-Id replay store per Stripe Idempotency-Key canon. 24h default TTL; background purge.';

-- ===========================================================================
-- buildingblocks schema + audit_log_entry
-- Cross-cutting audit log auto-written per command via Watermill router
-- AuditLoggingMiddleware. NOT tenant-RLS — operator queries cross every
-- tenant; per-tenant filter happens at query time via tenant_id column.
--
-- payload is jsonb (NOT bytea) so audit queries can index/filter on
-- specific event fields without round-trip deserialise.
--
-- Per `data-retention.md` "Audit log retention" — 7-year retention with
-- daily Hangfire job purging older rows. Pre-purge cold-storage export
-- to S3 Glacier per SOC2 CC4.1.
-- ===========================================================================

CREATE SCHEMA IF NOT EXISTS buildingblocks;
COMMENT ON SCHEMA buildingblocks IS
    'LeadKart cross-cutting plumbing tables (audit log, future cross-cutting documents).';

CREATE TABLE buildingblocks.audit_log_entry (
    id              uuid        PRIMARY KEY,
    action          text        NOT NULL CHECK (length(action) > 0 AND length(action) <= 200),
    user_id         uuid        NULL,         -- the acting Person; NULL on anonymous flows (e.g. login attempt)
    tenant_id       uuid        NULL,         -- scope; NULL on cross-tenant ops
    correlation_id  uuid        NULL,         -- request correlation chain
    occurred_at_utc timestamptz NOT NULL,
    duration_ms     bigint      NOT NULL,
    succeeded       boolean     NOT NULL,
    failure_reason  text        NULL,
    payload         jsonb       NULL          -- command/event-specific structured data
);

CREATE INDEX idx_audit_action_time
    ON buildingblocks.audit_log_entry (action, occurred_at_utc DESC);

CREATE INDEX idx_audit_user_time
    ON buildingblocks.audit_log_entry (user_id, occurred_at_utc DESC)
    WHERE user_id IS NOT NULL;

CREATE INDEX idx_audit_tenant_time
    ON buildingblocks.audit_log_entry (tenant_id, occurred_at_utc DESC)
    WHERE tenant_id IS NOT NULL;

COMMENT ON TABLE buildingblocks.audit_log_entry IS
    'Auto-written per command via Watermill AuditLoggingMiddleware. 7-year retention; daily purge.';

-- ===========================================================================
-- identity.processed_messages
-- Per-handler inbox dedup. The IdempotentReceiver middleware in the
-- Watermill router wraps each handler such that replaying the SAME
-- message_id against the SAME handler_name short-circuits to no-op.
--
-- Composite primary key (message_id, handler_name) means the same
-- message CAN be processed by multiple subscribers (one row per
-- (message, handler) pair) — but each (message, handler) combination
-- runs at most once.
--
-- Per `messaging.md` "Idempotency — inbox-side required" Layer 2.
-- ===========================================================================

-- arch-test:opt-out-rls (message-bus inbox dedup — cross-tenant by design.
--   Keyed by message_id+handler_name; isolation via natural-key uniqueness.)
CREATE TABLE identity.processed_messages (
    message_id       uuid        NOT NULL,
    handler_name     text        NOT NULL CHECK (length(handler_name) > 0 AND length(handler_name) <= 200),
    processed_at_utc timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, handler_name)
);

-- Background-purge index: rows older than the retention window
-- (typically 30 days, longer than any reasonable broker redelivery)
-- are deleted. Index supports the WHERE clause efficiently.
CREATE INDEX idx_processed_age ON identity.processed_messages (processed_at_utc);

COMMENT ON TABLE identity.processed_messages IS
    'Per-handler inbox dedup. (message_id, handler_name) PK guarantees at-most-once-per-handler delivery.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS identity.processed_messages;
DROP TABLE IF EXISTS buildingblocks.audit_log_entry CASCADE;
DROP SCHEMA IF EXISTS buildingblocks CASCADE;
DROP TABLE IF EXISTS app.command_idempotency;

-- +goose StatementEnd
