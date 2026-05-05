-- Refresh-token queries — both family and child-token tables are non-RLS
-- (session-management infrastructure). Token-hash uniqueness is the
-- load-bearing isolation per Auth0/Okta canon. tenant_id travels as
-- data column for context, not as RLS scope.

-- name: InsertRefreshTokenFamily :exec
INSERT INTO identity.refresh_token_families (
    id, person_id, tenant_id, device_label,
    created_at, last_used_at, revoked_at, revoke_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertRefreshToken :exec
INSERT INTO identity.refresh_tokens (
    id, family_id, token_hash, generation,
    issued_at, expires_at, consumed_at, replaced_by_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetRefreshTokenFamilyByID :one
SELECT id, person_id, tenant_id, device_label,
       created_at, last_used_at, revoked_at, revoke_reason
FROM   identity.refresh_token_families
WHERE  id = $1;

-- name: GetRefreshTokenByHash :one
-- Single indexed lookup by token hash — the rotation flow's entry point.
-- Returns the token row; caller resolves the family via family_id.
SELECT id, family_id, token_hash, generation,
       issued_at, expires_at, consumed_at, replaced_by_id
FROM   identity.refresh_tokens
WHERE  token_hash = $1;

-- name: ListRefreshTokensInFamily :many
SELECT id, family_id, token_hash, generation,
       issued_at, expires_at, consumed_at, replaced_by_id
FROM   identity.refresh_tokens
WHERE  family_id = $1
ORDER  BY generation;

-- name: UpdateRefreshTokenConsumed :exec
UPDATE identity.refresh_tokens
SET    consumed_at    = $2,
       replaced_by_id = $3
WHERE  id = $1;

-- name: UpdateRefreshTokenFamilyRevoked :exec
UPDATE identity.refresh_token_families
SET    revoked_at    = $2,
       revoke_reason = $3
WHERE  id = $1;

-- name: TouchRefreshTokenFamilyLastUsed :exec
UPDATE identity.refresh_token_families
SET    last_used_at = $2
WHERE  id = $1;
