-- LeadKart Go - a refresh-token grace replay belongs to the client that consumed the
-- token (fix round 13, item 3; ADR 0068 D10).
--
-- WHY. Inside the grace window (default 30 s) a just-rotated token handed the same
-- successor to ANYONE who presented it, unrecorded and unbounded: a thief who
-- obtained a token a moment before the victim used it, or who mirrored the BFF's
-- refresh cadence, rode the session without ever tripping reuse detection. A replay
-- is now accepted only from the client that consumed the token, at most
-- MaxGraceReplays (3) times; anything else is reuse (family revoked, 401).
--
-- WHAT. Two columns on identity.refresh_tokens, both about the token as a CONSUMED
-- token:
--   consumed_client_binding  text NULL. hex(HMAC-SHA256(client-binding key, label ||
--       family id || client key)): the client that consumed the token. NEVER an
--       address. The key is HKDF-SHA256 of the successor key (itself derived from the
--       JWT signing key) under the label "leadkart refresh-token client binding v1"
--       (refreshmint.DeriveClientBindingKey), so the database alone cannot be searched
--       for an address and the same client has unrelated values in unrelated families.
--       NULL on every row that exists today and on a token consumed by a request that
--       carried no client: such a token is STRICT (a presentation inside its window
--       counts as reuse), the safe direction for a migration.
--   grace_replays            smallint NOT NULL DEFAULT 0. Replays honoured for this
--       consumed token; incremented under the family row lock (the same lock rotation
--       takes), so concurrent presentations count exactly.
--
-- GRANTS. identity.refresh_tokens already carries the application role's table-level
-- DML; the new columns inherit it, so nothing to GRANT and the start-up privilege
-- check is unaffected.
--
-- LOCKING (final fix round). identity.refresh_tokens grows with every rotation, so
-- the CHECKs are added NOT VALID and validated by separate statements, not inline on
-- ADD COLUMN (an inline CHECK is verified by a scan under ACCESS EXCLUSIVE), and
-- SET LOCAL lock_timeout makes the migration fail fast, and roll back whole, when the
-- table is busy instead of queueing behind a long transaction and stalling every
-- refresh behind it. goose runs a file in one transaction, so the ACCESS EXCLUSIVE of
-- the DDL is held until the file commits whatever the order; the split keeps the
-- validation scan off the ADD COLUMN statement and is the shape to copy when a later
-- migration runs outside a transaction. No earlier migration sets a lock_timeout or
-- uses NOT VALID: migration 20261120000005 starts the convention.
--
-- Up adds both columns; Down drops them (the replay budget of a token still inside
-- its window is lost, nothing else depends on them).

-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE identity.refresh_tokens
    ADD COLUMN consumed_client_binding text     NULL,
    ADD COLUMN grace_replays           smallint NOT NULL DEFAULT 0;

ALTER TABLE identity.refresh_tokens
    ADD CONSTRAINT chk_refresh_tokens_client_binding CHECK (consumed_client_binding IS NULL OR length(consumed_client_binding) = 64) NOT VALID,
    ADD CONSTRAINT chk_refresh_tokens_grace_replays CHECK (grace_replays >= 0) NOT VALID;

ALTER TABLE identity.refresh_tokens
    VALIDATE CONSTRAINT chk_refresh_tokens_client_binding;
ALTER TABLE identity.refresh_tokens
    VALIDATE CONSTRAINT chk_refresh_tokens_grace_replays;

COMMENT ON COLUMN identity.refresh_tokens.consumed_client_binding IS 'hex(HMAC-SHA256) binding of the client that consumed this token; never an address. NULL = none recorded (strict: a replay is reuse). ADR 0068 D10.';
COMMENT ON COLUMN identity.refresh_tokens.grace_replays IS 'Grace replays honoured for this consumed token (at most 3), counted under the family row lock. ADR 0068 D10.';

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE identity.refresh_tokens
    DROP COLUMN IF EXISTS grace_replays,
    DROP COLUMN IF EXISTS consumed_client_binding;
