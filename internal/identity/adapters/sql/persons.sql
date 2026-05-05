-- Person queries — identity.persons is non-RLS (global identity).
-- Email is globally unique; lookup by email is the entry point for login
-- + password-reset + email-change flows per multi-tenancy.md "Identity model".

-- name: InsertPerson :exec
INSERT INTO identity.persons (
    id, email, first_name, last_name, password_hash, security_stamp,
    is_active, is_anonymised, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetPersonByID :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at
FROM   identity.persons
WHERE  id = $1;

-- name: GetPersonByEmail :one
SELECT id, email, first_name, last_name, password_hash, security_stamp,
       is_active, is_anonymised, created_at, anonymised_at
FROM   identity.persons
WHERE  email = $1;

-- name: UpdatePersonPassword :exec
UPDATE identity.persons
SET    password_hash  = $2,
       security_stamp = $3
WHERE  id = $1;

-- name: AnonymisePerson :exec
UPDATE identity.persons
SET    email          = $2,
       first_name     = '',
       last_name      = '',
       password_hash  = '',
       security_stamp = $3,
       is_active      = false,
       is_anonymised  = true,
       anonymised_at  = $4
WHERE  id = $1;
