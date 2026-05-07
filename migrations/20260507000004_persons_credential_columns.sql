-- LeadKart Go — Person credential-state columns.
--
-- Extends identity.persons with the columns the Person aggregate
-- already carries in its Snapshot but which the v0.2 schema didn't
-- yet round-trip:
--
--   - global suspension state (rare; compliance / fraud / abuse)
--   - pending password-reset (single-pending invariant per Auth0/Okta)
--   - pending email-change (single-pending invariant per Auth0/Okta)
--
-- Per `multi-tenancy.md` "Identity model": Person is global (NOT
-- tenant-scoped). All columns added below live on the global row;
-- the tenant context for a reset / email-change request is resolved
-- via the auth_routing index, not stored on the Person.
--
-- Single-pending discipline (canonical pattern):
--   - password_reset_token_hash  is NULL when no reset is in flight.
--   - pending_email_change_*     are NULL when no change is in flight.
--   - Re-issuing a reset / change replaces the pending row in-place
--     (the domain method enforces this). Old hash becomes
--     unreachable + falls out of validity by TTL.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.persons
    ADD COLUMN is_globally_suspended    boolean     NOT NULL DEFAULT false,
    ADD COLUMN global_suspension_reason text        NOT NULL DEFAULT '',
    ADD COLUMN globally_suspended_at    timestamptz NULL;

ALTER TABLE identity.persons
    ADD COLUMN password_reset_token_hash text        NULL,
    ADD COLUMN password_reset_expires_at timestamptz NULL,
    ADD CONSTRAINT chk_persons_password_reset_paired
        CHECK (
            (password_reset_token_hash IS NULL AND password_reset_expires_at IS NULL)
         OR (password_reset_token_hash IS NOT NULL AND password_reset_expires_at IS NOT NULL)
        );

ALTER TABLE identity.persons
    ADD COLUMN pending_email_change_new_email   text        NULL,
    ADD COLUMN pending_email_change_token_hash  text        NULL,
    ADD COLUMN pending_email_change_expires_at  timestamptz NULL,
    ADD CONSTRAINT chk_persons_email_change_paired
        CHECK (
            (pending_email_change_new_email IS NULL
             AND pending_email_change_token_hash IS NULL
             AND pending_email_change_expires_at IS NULL)
         OR (pending_email_change_new_email IS NOT NULL
             AND pending_email_change_token_hash IS NOT NULL
             AND pending_email_change_expires_at IS NOT NULL)
        );

-- Hash-only lookup index for the confirm flows. The reset / email-
-- change confirm endpoints receive the plaintext token, hash it, and
-- look up the Person by hash. UNIQUE because token hashes are
-- collision-resistant and a hash collision would let one Person's
-- token confirm another's pending change.
CREATE UNIQUE INDEX uq_persons_password_reset_hash
    ON identity.persons (password_reset_token_hash)
    WHERE password_reset_token_hash IS NOT NULL;

CREATE UNIQUE INDEX uq_persons_email_change_hash
    ON identity.persons (pending_email_change_token_hash)
    WHERE pending_email_change_token_hash IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.uq_persons_email_change_hash;
DROP INDEX IF EXISTS identity.uq_persons_password_reset_hash;

ALTER TABLE identity.persons
    DROP CONSTRAINT IF EXISTS chk_persons_email_change_paired,
    DROP CONSTRAINT IF EXISTS chk_persons_password_reset_paired;

ALTER TABLE identity.persons
    DROP COLUMN IF EXISTS pending_email_change_expires_at,
    DROP COLUMN IF EXISTS pending_email_change_token_hash,
    DROP COLUMN IF EXISTS pending_email_change_new_email,
    DROP COLUMN IF EXISTS password_reset_expires_at,
    DROP COLUMN IF EXISTS password_reset_token_hash,
    DROP COLUMN IF EXISTS globally_suspended_at,
    DROP COLUMN IF EXISTS global_suspension_reason,
    DROP COLUMN IF EXISTS is_globally_suspended;

-- +goose StatementEnd
