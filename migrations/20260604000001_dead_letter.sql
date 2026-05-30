-- LeadKart Go — dead-letter table (common cross-cutting infra).
-- Per ADR 0067 (canonical Watermill resilience): the PoisonQueue
-- middleware salvages messages that exhaust retries (or are marked
-- NonRetryable) to the `dead_letter` topic; the DeadLetterWriter
-- subscriber persists them here for inspection + manual replay. A
-- broker-side DLQ topic alone is ephemeral; this is the durable record.
--
-- Lives in `common` (the cross-cutting infra schema, ex-buildingblocks)
-- alongside audit_log_entry + command_idempotency.
--
-- +goose Up
-- +goose StatementBegin

CREATE TABLE common.dead_letter (
    id               uuid        PRIMARY KEY,
    topic            text        NOT NULL,           -- original destination topic
    handler_name     text        NOT NULL,           -- handler that gave up (poison metadata)
    message_id       text        NOT NULL,           -- original Watermill message UUID (replay key)
    reason           text        NOT NULL,           -- last error / poison reason
    payload          bytea       NOT NULL,           -- raw original payload (may be malformed; bytea preserves it for replay)
    metadata         jsonb       NOT NULL,           -- original message metadata (string map)
    dead_lettered_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_dead_letter_topic_time
    ON common.dead_letter (topic, dead_lettered_at DESC);
CREATE INDEX idx_dead_letter_handler_time
    ON common.dead_letter (handler_name, dead_lettered_at DESC);

COMMENT ON TABLE common.dead_letter IS 'Durable landing zone for poisoned messages (retries exhausted or NonRetryable). Inspect + replay. Per ADR 0067.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS common.dead_letter;

-- +goose StatementEnd
