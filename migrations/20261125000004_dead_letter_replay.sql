-- Review G2b H1: a handler attempt that times out is retried at most once on the
-- delivery lane and then dead-lettered, so a stuck event cannot hold the lane for
-- every company. A dead letter that failed by timeout is delivered again, off the
-- lane, by the dead-letter replay job (bounded, with backoff).
--
-- Each replay is ONE row here. common.dead_letter is not touched: the dead letter
-- and every replay of it stay for good (ADR 0071: we never delete any data). The
-- table is append-only for the application (SELECT, INSERT), like the audit trail.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE common.dead_letter_replay (
    id             uuid        PRIMARY KEY,
    dead_letter_id uuid        NOT NULL UNIQUE REFERENCES common.dead_letter (id),
    message_id     text        NOT NULL,
    handler_name   text        NOT NULL,
    replayed_at    timestamptz NOT NULL
);

CREATE INDEX idx_dead_letter_replay_pair
    ON common.dead_letter_replay (message_id, handler_name);

COMMENT ON TABLE common.dead_letter_replay IS
    'One row per replay of a timed-out dead letter (review G2b H1). Append-only and kept forever (ADR 0071): the application role holds SELECT and INSERT only.';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        REVOKE UPDATE, DELETE ON common.dead_letter_replay FROM leadkart_app;
    END IF;
END
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS common.dead_letter_replay;

-- +goose StatementEnd
