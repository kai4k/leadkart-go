-- Person queries — identity.persons is non-RLS (global identity).
-- Email is globally unique; lookup by email is the entry point for login
-- + password-reset + email-change flows per multi-tenancy.md "Identity model".

-- name: InsertPerson :exec
INSERT INTO identity.persons (
    id, email, first_name, last_name, password_hash, security_stamp,
    is_active, is_anonymised, created_at,
    is_globally_suspended, global_suspension_reason, globally_suspended_at,
    password_reset_token_hash, password_reset_expires_at,
    pending_email_change_new_email, pending_email_change_token_hash,
    pending_email_change_expires_at,
    created_by_person_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9,
    $10, $11, $12,
    $13, $14,
    $15, $16, $17,
    $18
);

-- name: GetPersonByID :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id
FROM   identity.persons
WHERE  id = $1;

-- name: GetPersonByEmail :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id
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
       p.is_anonymised,
       p.created_at                      AS person_created_at,
       p.anonymised_at,
       p.is_globally_suspended,
       p.global_suspension_reason,
       p.globally_suspended_at,
       p.password_reset_token_hash,
       p.password_reset_expires_at,
       p.pending_email_change_new_email,
       p.pending_email_change_token_hash,
       p.pending_email_change_expires_at,
       p.created_by_person_id,
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
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id
FROM   identity.persons
WHERE  password_reset_token_hash = $1;

-- name: GetPersonByEmailChangeTokenHash :one
-- Hash-only lookup for the confirm-email-change flow.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at,
       created_by_person_id
FROM   identity.persons
WHERE  pending_email_change_token_hash = $1;

-- name: UpdatePerson :exec
-- General-purpose update covering ChangePassword + Anonymise + global
-- suspension / lift + password-reset request/confirm/cancel + email-
-- change request/confirm/cancel + future mutations. Repository writes
-- whatever the aggregate currently says.
UPDATE identity.persons
SET    email                            = $2,
       first_name                       = $3,
       last_name                        = $4,
       password_hash                    = $5,
       security_stamp                   = $6,
       is_active                        = $7,
       is_anonymised                    = $8,
       anonymised_at                    = $9,
       is_globally_suspended            = $10,
       global_suspension_reason         = $11,
       globally_suspended_at            = $12,
       password_reset_token_hash        = $13,
       password_reset_expires_at        = $14,
       pending_email_change_new_email   = $15,
       pending_email_change_token_hash  = $16,
       pending_email_change_expires_at  = $17
WHERE  id = $1;


-- name: SearchPersonsByText :many
-- Cross-tenant person search per ADR 0040 (pg_trgm). Backed by
-- idx_persons_search_trgm (GIN over lower(email||' '||first_name||
-- ' '||last_name) WHERE is_active AND NOT is_anonymised).
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
           lower($1)
       ) AS rank
FROM   identity.persons
WHERE  is_active AND NOT is_anonymised
AND    (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name))
       ILIKE '%' || lower($1) || '%'
ORDER  BY rank DESC, id DESC
LIMIT  $2;
