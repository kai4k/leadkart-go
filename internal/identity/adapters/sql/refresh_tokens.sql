-- Refresh-token queries — both family and child-token tables are non-RLS
-- (session-management infrastructure). Token-hash uniqueness is the
-- load-bearing isolation per Auth0/Okta canon. tenant_id travels as
-- data column for context, not as RLS scope.

-- name: InsertRefreshTokenFamily :exec
INSERT INTO identity.refresh_token_families (
    id, person_id, tenant_id, device_label,
    created_at, last_used_at, revoked_at, revoke_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpsertRefreshToken :exec
-- INSERT-then-UPDATE-on-conflict by token id. The family aggregate
-- emits the FULL token list on persist; rows whose (consumed_at,
-- replaced_by_id) changed are updated, fresh tokens are inserted.
-- Token id, hash, generation, issued_at, expires_at, family_id are
-- immutable post-issuance; only the consumed columns rotate.
INSERT INTO identity.refresh_tokens (
    id, family_id, token_hash, generation,
    issued_at, expires_at, consumed_at, replaced_by_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET
    consumed_at    = EXCLUDED.consumed_at,
    replaced_by_id = EXCLUDED.replaced_by_id;

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

-- name: UpdateRefreshTokenFamily :exec
-- Persists family-level mutable state: last_used_at on every rotate;
-- revoked_at + revoke_reason on Revoke or reuse-detected.
UPDATE identity.refresh_token_families
SET    last_used_at  = $2,
       revoked_at    = $3,
       revoke_reason = $4
WHERE  id = $1;
