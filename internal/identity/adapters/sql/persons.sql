-- Person queries — identity.persons is non-RLS (global identity).
-- Email is globally unique; lookup by email is the entry point for login
-- + password-reset + email-change flows per multi-tenancy.md "Identity model".

-- name: InsertPerson :exec
INSERT INTO identity.persons (
    id, email, first_name, last_name, password_hash, security_stamp,
    is_active, created_at,
    is_globally_suspended, global_suspension_reason, globally_suspended_at,
    password_reset_token_hash, password_reset_expires_at,
    pending_email_change_new_email, pending_email_change_token_hash,
    pending_email_change_expires_at,
    created_by_person_id,
    must_change_password, failed_login_count, locked_until, last_failed_login_at,
    password_reset_failed_attempts
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11,
    $12, $13,
    $14, $15, $16,
    $17,
    $18, $19, $20, $21,
    $22
);

-- name: GetPersonByID :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  id = $1;

-- name: GetPersonByIDForUpdate :one
-- The aggregate-writer load (UpdateByID): takes the row lock so every writer of
-- a person serialises on it (UpdatePerson rewrites every column from the
-- loaded snapshot, so an unlocked load can write back a stale password hash,
-- stamp or suspension). FOR NO KEY UPDATE: conflicts with other writers but not
-- with the FOR KEY SHARE a foreign-key check takes (memberships, refresh
-- families referencing the person).
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  id = $1
FOR NO KEY UPDATE;

-- name: GetPersonByIDForShare :one
-- The login's session-insert check (fix round 12, item 1): reads the credential
-- under a SHARE lock in the transaction that inserts the new refresh family.
-- FOR SHARE conflicts with the FOR NO KEY UPDATE every password writer takes, so
-- the read waits for a change that is committing and then sees its result; a change
-- that starts afterwards waits for this transaction (which ends right after the
-- family insert) and then revokes the family it finds. Two logins of one person
-- share the lock and do not serialise. Lock order: person (share) -> the NEW family
-- row; nothing in this transaction locks an existing family or takes a stronger
-- person lock (the lockout writes run in their own transactions, before and after).
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  id = $1
FOR SHARE;

-- name: GetPersonsByIDs :many
-- Batched hydration — single round-trip via `id = ANY(...)` instead of
-- N GetPersonByID calls. Column list mirrors GetPersonByID exactly so
-- it reuses the IdentityPerson model.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetPersonByEmail :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  email = $1;

-- name: GetPersonAndActiveMembershipByEmail :one
-- Single-roundtrip auth-routing lookup for the login flow. Joins
-- identity.persons (global, non-RLS) to the Person's at-most-one
-- Active Membership via the partial-unique index
-- `uq_memberships_person_active`. LEFT JOIN so a Person without an
-- Active Membership still surfaces — the login handler maps that
-- to the same generic invalid_credentials response.
--
-- All membership_* columns are nullable in the result; sqlc maps
-- those to *pgtype.UUID / *pgtype.Text via emit_pointers_for_null_types.
--
-- Roles + permission overrides are fetched separately by the caller
-- (different access pattern, different caching story). This query
-- saves the persons→memberships network roundtrip — the dominant
-- modern-canon optimisation per Brandon Mitchell / Brandur Leach
-- "Postgres scales further than you think". Materialised views or
-- denormalised auth_routing tables (Stripe-2014 / Auth0 patterns)
-- are the next escalation; we don't need them at this scale.
SELECT p.id                              AS person_id,
       p.email,
       p.first_name,
       p.last_name,
       p.password_hash,
       p.security_stamp,
       p.is_active,
       p.created_at                      AS person_created_at,
       p.is_globally_suspended,
       p.global_suspension_reason,
       p.globally_suspended_at,
       p.password_reset_token_hash,
       p.password_reset_expires_at,
       p.pending_email_change_new_email,
       p.pending_email_change_token_hash,
       p.pending_email_change_expires_at,
       p.created_by_person_id,
       p.must_change_password,
       p.failed_login_count,
       p.locked_until,
       p.last_failed_login_at,
       p.password_reset_failed_attempts,
       m.id                              AS membership_id,
       m.tenant_id,
       m.status                          AS membership_status,
       m.joined_at,
       m.left_at,
       m.designation,
       m.department,
       m.status_message,
       m.reports_to,
       m.created_by_membership_id
FROM   identity.persons p
LEFT   JOIN identity.tenant_memberships m
       ON  m.person_id = p.id AND m.status = 'active'
WHERE  p.email = $1;

-- name: GetPersonByPasswordResetTokenHash :one
-- Hash-only lookup for the confirm-password-reset flow.
-- Caller hashes the plaintext + queries here; UNIQUE index makes this
-- a single-row lookup.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  password_reset_token_hash = $1;

-- name: GetPersonByEmailChangeTokenHash :one
-- Hash-only lookup for the confirm-email-change flow.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, created_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id,
       must_change_password, failed_login_count, locked_until, last_failed_login_at,
       password_reset_failed_attempts
FROM   identity.persons
WHERE  pending_email_change_token_hash = $1;

-- name: UpdatePerson :exec
-- General-purpose update covering ChangePassword + global
-- suspension / lift + password-reset request/confirm/cancel + email-
-- change request/confirm/cancel + lockout-state transitions on the
-- self-contained Login success path + future mutations. Repository
-- writes whatever the aggregate currently says.
UPDATE identity.persons
SET    email                            = $2,
       first_name                       = $3,
       last_name                        = $4,
       password_hash                    = $5,
       security_stamp                   = $6,
       is_active                        = $7,
       is_globally_suspended            = $8,
       global_suspension_reason         = $9,
       globally_suspended_at            = $10,
       password_reset_token_hash        = $11,
       password_reset_expires_at        = $12,
       pending_email_change_new_email   = $13,
       pending_email_change_token_hash  = $14,
       pending_email_change_expires_at  = $15,
       must_change_password             = $16,
       failed_login_count               = $17,
       locked_until                     = $18,
       last_failed_login_at             = $19,
       password_reset_failed_attempts   = $20
WHERE  id = $1;

-- name: RegisterPersonSuccessfulLogin :one
-- The success half of the lockout (ADR 0068 D12). A login registers its attempt
-- BEFORE it verifies (RegisterPersonFailedLogin), so a correct password has to
-- undo that count - and it must never undo a LOCK: the row is locked, and the
-- counters are cleared only when no lock is in force at sqlc.arg(now) - or the lock
-- in force is sqlc.narg(own_lock), the one this very attempt's registration set when
-- it crossed the threshold (NULL for every other attempt), so that nine wrong
-- passwords plus the right one still sign in while a right password arriving after
-- SOMEONE ELSE's lock never does. Zero
-- cleared = a concurrent failure locked the account while this attempt was being
-- verified; the caller answers 423 with the returned lock and opens no session.
-- Returns what the row held before the clear, so the caller can tell a real
-- unlock from the attempt's own count.
WITH cur AS (
    SELECT t.id, t.failed_login_count, t.locked_until
    FROM   identity.persons AS t
    WHERE  t.id = sqlc.arg(id)
    FOR NO KEY UPDATE OF t
), cleared AS (
    UPDATE identity.persons p
    SET    failed_login_count   = 0,
           locked_until         = NULL,
           last_failed_login_at = NULL
    FROM   cur
    WHERE  p.id = cur.id
      AND  (cur.locked_until IS NULL
            OR cur.locked_until <= sqlc.arg(now)::timestamptz
            OR cur.locked_until = sqlc.narg(own_lock)::timestamptz)
    RETURNING p.id
)
SELECT cur.failed_login_count AS prev_failed_count,
       cur.locked_until       AS prev_locked_until,
       EXISTS (SELECT 1 FROM cleared) AS cleared
FROM   cur;


-- name: RegisterPersonFailedLogin :one
-- ONE atomic statement decides the failed-login count and the lock (ADR 0068
-- D12). The old flow read the row, bumped the counter in Go ~30 ms of argon2
-- later and wrote an absolute value, so concurrent wrong passwords lost updates
-- and a batch could guess past the lock. Here the row is locked FOR NO KEY UPDATE
-- (compatible with the key-share lock a login's refresh-family insert takes; it conflicts with the FOR SHARE the login's session insert takes, which is why the two never share a transaction), the
-- new count and lock are computed from the locked version, and the caller decides
-- "locked" from the RETURNING row.
--
-- Mirrors person.RegisterFailedLogin exactly (a parity test pins it): the window
-- resets the count to 1 when the previous failure is OLDER than sqlc.arg(window_secs); a
-- lock still in force is kept; otherwise reaching sqlc.arg(max_failed) locks for
-- sqlc.arg(lock_secs); an expired lock is cleared only when the window has elapsed.
-- prev_locked_until lets the repository emit the account-locked event only for
-- the attempt that actually crossed the threshold.
UPDATE identity.persons p
SET    failed_login_count   = c.cnt,
       last_failed_login_at = sqlc.arg(now)::timestamptz,
       locked_until         = CASE
           WHEN c.prev_locked > sqlc.arg(now)::timestamptz THEN c.prev_locked
           WHEN c.cnt >= sqlc.arg(max_failed)::int         THEN sqlc.arg(now)::timestamptz + (sqlc.arg(lock_secs)::float8 * interval '1 second')
           WHEN c.elapsed                         THEN NULL
           ELSE c.prev_locked
       END
FROM (
    SELECT s.id, s.prev_locked, s.elapsed,
           CASE WHEN s.elapsed THEN 1 ELSE s.cur_count + 1 END AS cnt
    FROM (
        SELECT cur.id,
               cur.locked_until       AS prev_locked,
               cur.failed_login_count AS cur_count,
               (cur.last_failed_login_at IS NOT NULL
                AND cur.last_failed_login_at < sqlc.arg(now)::timestamptz - (sqlc.arg(window_secs)::float8 * interval '1 second')) AS elapsed
        FROM   identity.persons AS cur
        WHERE  cur.id = sqlc.arg(id)
        FOR NO KEY UPDATE OF cur
    ) s
) c
WHERE  p.id = c.id
RETURNING p.failed_login_count, p.last_failed_login_at, p.locked_until, c.prev_locked AS prev_locked_until;


-- name: SearchPersonsByText :many
-- Cross-tenant person search per ADR 0040 (pg_trgm). Backed by
-- idx_persons_search_trgm (GIN over lower(email||' '||first_name||
-- ' '||last_name) WHERE is_active).
--
-- Operator-only path (caller must run under TxScopePlatform).
-- similarity() ranking — closer trigram overlap surfaces first.
-- Caller-supplied query string is the raw operator input;
-- callers MUST bound length at the HTTP boundary (2-100 chars
-- per the omni-search contract) — anything outside is rejected
-- 400 before reaching this query.
SELECT id, email, first_name, last_name, created_at,
       similarity(
           lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name),
           lower(sqlc.arg(query))
       ) AS rank
FROM   identity.persons
WHERE  is_active
AND    (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name))
       ILIKE '%' || lower(sqlc.arg(query)) || '%'
ORDER  BY rank DESC, id DESC
LIMIT  sqlc.arg('limit');
