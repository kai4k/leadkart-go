-- LeadKart Go — per-caller idempotency key scoping + response-header capture.
--
-- Stripe canon (https://stripe.com/blog/idempotency 2018): "An idempotency
-- key is scoped to the API key" — which is to say *per caller*, not
-- globally. Without per-caller scoping the same X-Command-Id from
-- Tenant A maps to a row stored by Tenant B; A then either:
--   1) gets back B's response body verbatim (cross-tenant data leak,
--      direct GDPR Art. 5 §1(f) integrity-and-confidentiality breach), OR
--   2) sees a 422 idempotency.key_reuse on a key it never used (DoS).
--
-- Both are critical correctness bugs the in-memory store didn't have
-- (single-process; no cross-tenant key collision in practice). The
-- Postgres-backed store + the original migration's `command_id PRIMARY
-- KEY` shape regresses on this — fix before wiring PostgresStore.
--
-- Additive ALTER: drop singleton PK + add caller_id + response_headers
-- + composite (caller_id, command_id) PK. Postgres handles ADD COLUMN
-- NOT NULL DEFAULT as fast metadata-only since 11; the trailing DROP
-- DEFAULT keeps callers honest going forward.
--
-- response_headers (jsonb) replaces the lossy "just bytea" capture so
-- Content-Type + downstream-set headers (X-Request-Id, ETag) survive
-- the replay round-trip.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE app.command_idempotency
    DROP CONSTRAINT command_idempotency_pkey;

ALTER TABLE app.command_idempotency
    ADD COLUMN caller_id        text  NOT NULL DEFAULT '',
    ADD COLUMN response_headers jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE app.command_idempotency
    ALTER COLUMN caller_id DROP DEFAULT;

ALTER TABLE app.command_idempotency
    ADD CONSTRAINT command_idempotency_pkey
        PRIMARY KEY (caller_id, command_id);

ALTER TABLE app.command_idempotency
    ADD CONSTRAINT command_idempotency_caller_id_nonempty
        CHECK (length(caller_id) > 0 AND length(caller_id) <= 200);

COMMENT ON COLUMN app.command_idempotency.caller_id IS
    'Per-caller scoping per Stripe Idempotency-Key canon. Tenant ID for tenant requests, "platform:<user>" for operator paths, "anon:<ip>" for unauth (defense-in-depth — middleware should not be on unauth routes).';

COMMENT ON COLUMN app.command_idempotency.response_headers IS
    'Captured response headers for verbatim replay (Content-Type minimum; ETag / X-Request-Id when set).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE app.command_idempotency
    DROP CONSTRAINT command_idempotency_caller_id_nonempty;

ALTER TABLE app.command_idempotency
    DROP CONSTRAINT command_idempotency_pkey;

ALTER TABLE app.command_idempotency
    DROP COLUMN response_headers,
    DROP COLUMN caller_id;

ALTER TABLE app.command_idempotency
    ADD CONSTRAINT command_idempotency_pkey PRIMARY KEY (command_id);

-- +goose StatementEnd
