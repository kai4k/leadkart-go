-- LeadKart Go - per-token bound on refused reset passwords (fix round 13, item 2).
--
-- WHY. A reset link with a valid token may be presented with a password the
-- policy or the breach list refuses; the token stays usable (the person may
-- simply try again) but not forever: after 5 refusals the token is invalidated
-- (command.PasswordResetMaxAttempts), so a holder of a leaked link cannot probe
-- the password rules indefinitely. The count belongs to the pending reset and
-- dies with it.
--
-- WHAT. One column on identity.persons next to password_reset_token_hash /
-- password_reset_expires_at: the number of refused presentations of the CURRENT
-- pending reset. A new reset request resets it to 0 (the aggregate writes a fresh
-- pending reset); a confirmed, cancelled or expired reset leaves it at 0 as well
-- (the aggregate clears the whole pending state and the repository writes 0).
-- NOT NULL DEFAULT 0 backfills existing rows to 0 and needs no table rewrite on a
-- constant default.
--
-- GRANTS. identity.persons already carries the application role's table-level
-- DML (20260606000001 + default privileges); a new column inherits it, so no
-- GRANT is needed and the start-up privilege check (pg.VerifyApplicationPrivileges)
-- is unaffected.
--
-- Up adds the column; Down drops it (the count is advisory state, losing it only
-- gives a token a fresh set of attempts).

-- +goose Up
ALTER TABLE identity.persons
    ADD COLUMN password_reset_failed_attempts integer NOT NULL DEFAULT 0
        CONSTRAINT chk_persons_password_reset_failed_attempts CHECK (password_reset_failed_attempts >= 0);

COMMENT ON COLUMN identity.persons.password_reset_failed_attempts IS 'Refused presentations of the pending password-reset token (policy / breach list); the token is invalidated at command.PasswordResetMaxAttempts. 0 when no reset is pending.';

-- +goose Down
ALTER TABLE identity.persons
    DROP COLUMN IF EXISTS password_reset_failed_attempts;
