-- name: RegisterPhantomLoginFailure :one
-- The failed-login counter of an address with no usable account: the same
-- atomic statement shape as RegisterPersonFailedLogin (persons.sql), so unknown
-- and real addresses do the same work and behave the same under concurrency.
-- key is HMAC-SHA-256(server-derived key, canonical email), never an email.
--
-- First sight inserts count 1 (the lock threshold is far above 1, so a fresh row
-- is never locked). On conflict the row lock held by ON CONFLICT DO UPDATE
-- serialises concurrent attempts, and the SET expressions see the committed row.
INSERT INTO identity.login_phantom_failures AS t (key, failed_count, last_failed_at, locked_until)
VALUES (sqlc.arg(key), 1, sqlc.arg(now)::timestamptz, NULL)
ON CONFLICT (key) DO UPDATE SET
    failed_count   = CASE
        WHEN t.last_failed_at < sqlc.arg(now)::timestamptz - (sqlc.arg(window_secs)::float8 * interval '1 second') THEN 1
        ELSE t.failed_count + 1
    END,
    last_failed_at = sqlc.arg(now)::timestamptz,
    locked_until   = CASE
        WHEN t.locked_until > sqlc.arg(now)::timestamptz THEN t.locked_until
        WHEN (CASE WHEN t.last_failed_at < sqlc.arg(now)::timestamptz - (sqlc.arg(window_secs)::float8 * interval '1 second')
                   THEN 1 ELSE t.failed_count + 1 END) >= sqlc.arg(max_failed)::int
            THEN sqlc.arg(now)::timestamptz + (sqlc.arg(lock_secs)::float8 * interval '1 second')
        WHEN t.last_failed_at < sqlc.arg(now)::timestamptz - (sqlc.arg(window_secs)::float8 * interval '1 second') THEN NULL
        ELSE t.locked_until
    END
RETURNING t.failed_count, t.last_failed_at, t.locked_until;

-- name: GetPhantomLoginLock :one
-- The active lock of an address with no usable account. Always one row (a scalar
-- subquery), NULL when the address is not locked, so "not locked" is a value and
-- not a missing-row error.
SELECT (
    SELECT locked_until
    FROM   identity.login_phantom_failures
    WHERE  key = sqlc.arg(key)
      AND  locked_until > sqlc.arg(now)::timestamptz
)::timestamptz AS locked_until;

-- name: PurgePhantomLoginFailures :execrows
-- Deletes a bounded batch of rows whose window AND lock have both passed.
-- sqlc.arg(window_cutoff) is now minus the lockout window; sqlc.arg(now) ends any lock.
DELETE FROM identity.login_phantom_failures
WHERE key IN (
    SELECT key
    FROM   identity.login_phantom_failures
    WHERE  last_failed_at < sqlc.arg(window_cutoff)::timestamptz
      AND  (locked_until IS NULL OR locked_until <= sqlc.arg(now)::timestamptz)
    ORDER  BY last_failed_at
    LIMIT  sqlc.arg(batch_size)::int
    FOR UPDATE SKIP LOCKED
);
