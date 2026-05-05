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

-- name: UpdatePerson :exec
-- General-purpose update covering ChangePassword + Anonymise + future
-- mutations. Repository decides which fields actually changed; SQL
-- writes whatever the aggregate currently says.
UPDATE identity.persons
SET    email          = $2,
       first_name     = $3,
       last_name      = $4,
       password_hash  = $5,
       security_stamp = $6,
       is_active      = $7,
       is_anonymised  = $8,
       anonymised_at  = $9
WHERE  id = $1;
