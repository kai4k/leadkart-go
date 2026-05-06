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
    pending_email_change_expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9,
    $10, $11, $12,
    $13, $14,
    $15, $16, $17
);

-- name: GetPersonByID :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at
FROM   identity.persons
WHERE  id = $1;

-- name: GetPersonByEmail :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at
FROM   identity.persons
WHERE  email = $1;

-- name: GetPersonByPasswordResetTokenHash :one
-- Hash-only lookup for the confirm-password-reset flow.
-- Caller hashes the plaintext + queries here; UNIQUE index makes this
-- a single-row lookup.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at
FROM   identity.persons
WHERE  password_reset_token_hash = $1;

-- name: GetPersonByEmailChangeTokenHash :one
-- Hash-only lookup for the confirm-email-change flow.
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at,
       is_globally_suspended, global_suspension_reason, globally_suspended_at,
       password_reset_token_hash, password_reset_expires_at,
       pending_email_change_new_email, pending_email_change_token_hash,
       pending_email_change_expires_at
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
