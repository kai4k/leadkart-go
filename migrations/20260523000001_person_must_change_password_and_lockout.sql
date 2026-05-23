-- LeadKart Go — Wave 9.2 — Person must_change_password + account lockout
--
-- Two BRD-aligned auth-hardening features on identity.persons:
--
--   1. must_change_password (BRD line 241) — true when an admin/operator
--      provisioned the credential (RegisterTenant admin, CreateUser by
--      admin). User is forced through the change-password flow on first
--      login. Cleared on self-changed/self-reset paths. Replaces the
--      email-verification-on-registration pattern (which doesn't fit
--      our B2B operator-onboarded model).
--
--   2. failed_login_count + locked_until + last_failed_login_at — NIST
--      800-63B §5.2.2 anti-brute-force per-account counter. 10 failed
--      attempts in a 15-minute sliding window → locked for 15 minutes.
--      Lockout is checked BEFORE bcrypt verify to avoid leaking
--      existence via timing. Counter cleared on successful login OR on
--      lockout expiry (sliding window).
--
-- identity.persons is non-RLS (global identity), so no policy edits.
--
-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.persons
    ADD COLUMN must_change_password   boolean     NOT NULL DEFAULT false,
    ADD COLUMN failed_login_count     int         NOT NULL DEFAULT 0,
    ADD COLUMN locked_until           timestamptz NULL,
    ADD COLUMN last_failed_login_at   timestamptz NULL;

COMMENT ON COLUMN identity.persons.must_change_password IS
    'BRD line 241 — true when admin/operator provisioned the credential; cleared on self-change/self-reset. Login still succeeds (frontend redirects); strict middleware enforcement is a v0.3 follow-up.';
COMMENT ON COLUMN identity.persons.failed_login_count IS
    'NIST 800-63B §5.2.2 — count of consecutive failed login attempts in the current sliding window. Reset to 0 on successful login OR when LockoutWindow elapses since last_failed_login_at.';
COMMENT ON COLUMN identity.persons.locked_until IS
    'NIST 800-63B §5.2.2 — set to now+LockoutDuration when failed_login_count reaches MaxFailedLogins. NULL once login has succeeded (or never been locked).';
COMMENT ON COLUMN identity.persons.last_failed_login_at IS
    'Timestamp of the most recent failed login; drives the sliding-window counter reset. NULL when failed_login_count = 0.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.persons
    DROP COLUMN IF EXISTS last_failed_login_at,
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS failed_login_count,
    DROP COLUMN IF EXISTS must_change_password;

-- +goose StatementEnd
