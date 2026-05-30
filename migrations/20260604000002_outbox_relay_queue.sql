-- LeadKart Go — transactional outbox as a watermill-sql relay queue.
--
-- Per ADR 0064 + ADR 0067: the outbox is a PURE RELAY (not the audit log,
-- not per-tenant state), so it is ONE shared platform-infrastructure table
-- with NO RLS — the destination module topic + tenant_id + occurred_at +
-- act_* all travel inside the forwarder envelope / message metadata, not as
-- queryable columns.
--
-- This replaces the four per-module outbox tables (identity/platform/crm/
-- inventory .outbox) + their bespoke `forwarded` flag + the four hand-rolled
-- OutboxForwarder structs. The producer publishes via watermill-sql's
-- PostgreSQLQueueSchema (forwarder.NewPublisher → sql.NewPublisher on the
-- aggregate tx); one Watermill Forwarder drains this table and republishes
-- to the destination broker; rows are DELETED on ack (DeleteOnAck).
--
-- Column shape MUST match watermill-sql PostgreSQLQueueSchema exactly
-- (offset / uuid / payload / metadata / acked / created_at). We create it
-- here so the schema is owned by goose (greppable, migration-gated) and the
-- relay table exists before the worker boots.
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE common.outbox (
    "offset"   BIGSERIAL   PRIMARY KEY,
    uuid       VARCHAR(36) NOT NULL,
    payload    JSON        DEFAULT NULL,
    metadata   JSON        DEFAULT NULL,
    acked      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Drain query is `WHERE acked = false ORDER BY "offset" ASC ... FOR UPDATE`;
-- a partial index on the unacked frontier keeps it index-only as the table
-- churns (rows are deleted on ack, so the live set stays small).
CREATE INDEX idx_outbox_unacked ON common.outbox ("offset") WHERE NOT acked;

COMMENT ON TABLE common.outbox IS 'Transactional-outbox relay queue (watermill-sql PostgreSQLQueueSchema). One shared table, no RLS — pure relay per ADR 0064/0067. Rows deleted on ack.';

-- Retire the four per-module outbox tables + their indexes/policies (CASCADE
-- drops the RLS policies). Their events now flow through common.outbox.
DROP TABLE IF EXISTS identity.outbox  CASCADE;
DROP TABLE IF EXISTS platform.outbox  CASCADE;
DROP TABLE IF EXISTS crm.outbox       CASCADE;
DROP TABLE IF EXISTS inventory.outbox CASCADE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS common.outbox;

-- Down does NOT recreate the per-module outbox tables — this migration is a
-- one-way relay consolidation (ADR 0064). Rolling back means restoring from
-- the prior migration set on a fresh database (the pre-production reset
-- workflow this repo uses).

-- +goose StatementEnd
