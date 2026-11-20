-- LeadKart Go - failed-login counter for addresses with no usable account (ADR 0068 D12).
--
-- WHY. A login for an unknown, deactivated or anonymised address must lock after
-- the same ten failures within the same window as a real account, or a 423 would
-- tell an attacker which emails exist. The counter used to live in process memory
-- (lost on restart, per replica, with a cap that failed open when full) and was
-- updated under a mutex while the real account's counter was a lost-update
-- read-modify-write: ten simultaneous wrong logins gave one 423 for an unknown
-- address and ten 401s for a real one. Both counters are now ONE atomic SQL
-- statement of the same shape (persons: RegisterPersonFailedLogin, here:
-- RegisterPhantomLoginFailure), so real and unknown addresses do the same work
-- (one row write per failed attempt), survive restarts alike and behave alike
-- under concurrency.
--
-- WHAT IS STORED. key is HMAC-SHA-256(server-derived key, canonical email): the
-- table never holds an email, a plain hash of one, or anything an attacker could
-- recompute without the server secret (the key derives from the JWT signing
-- secret with a fixed label, see adapters.NewPhantomLockoutRepository). Rotating
-- that secret simply restarts the counters, which live for 15 minutes anyway.
-- failed_count / last_failed_at / locked_until carry exactly the semantics of the
-- identically named identity.persons columns.
--
-- RLS POSITION. There is no tenant to scope by: the row is about an address, not
-- a tenant, and the table is reachable only through the login path (the two
-- statements in internal/identity/adapters/sql/phantom_lockout.sql, run under the
-- platform transaction scope). It follows identity.persons and
-- identity.refresh_token_families: no row-level security, isolation by the
-- application's access path; the application role gets DML on it and nothing
-- else. The start-up privilege check (pg.VerifyApplicationPrivileges) covers it
-- like every table of the identity schema.
--
-- GROWTH. A failing attempt touches one row; a row whose 15-minute window and
-- lock have both passed is dead weight. A River periodic job
-- (identity.login_phantom_purge, every 5 minutes) deletes such rows in bounded
-- batches. Worst case (theoretical ceiling): the per-client login limiter admits
-- 30 attempts a minute per source and holds at most 50,000 sources per process,
-- and a row lives at most 15 minutes of window plus up to 5 minutes until the
-- next purge: 50,000 x 30 x 20 = 30 million rows (about 4.5 GB with its index),
-- which needs 50,000 distinct /64 or IPv4 sources each sustaining the limit for
-- twenty minutes. A realistic botnet of 1,000 sources is 600,000 rows (about
-- 90 MB), purged within 20 minutes of the attack ending.
--
-- Down drops the table; the counters restart empty, nothing else depends on it.

-- +goose Up
-- arch-test:opt-out-rls (identity.login_phantom_failures is keyed by an HMAC of an
--   email address, not by a tenant; it is read and written only by the login path
--   under the platform scope. Same position as identity.persons and
--   identity.refresh_token_families. See the RLS POSITION paragraph above.)
CREATE TABLE identity.login_phantom_failures (
    key            bytea       PRIMARY KEY CHECK (length(key) = 32),
    failed_count   integer     NOT NULL CHECK (failed_count >= 1),
    last_failed_at timestamptz NOT NULL,
    locked_until   timestamptz NULL
);

COMMENT ON TABLE identity.login_phantom_failures IS 'Failed-login counter for addresses with no usable account. key = HMAC-SHA-256(server key, canonical email): never an email. Same atomic statement shape as identity.persons lockout. Purged by identity.login_phantom_purge. ADR 0068 D12.';

-- The purge scans by age.
CREATE INDEX idx_login_phantom_failures_last_failed
    ON identity.login_phantom_failures (last_failed_at);

-- The application role gets DML here explicitly (default privileges of the
-- migration owner already cover new identity tables; this makes the migration
-- self-contained). Guarded like 20260606000001: a no-op where the role is absent.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON identity.login_phantom_failures TO leadkart_app;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS identity.login_phantom_failures;
