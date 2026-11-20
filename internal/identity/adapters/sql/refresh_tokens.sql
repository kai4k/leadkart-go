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
-- immutable post-issuance; only the consumed columns (consumed_at, replaced_by_id,
-- the client binding and the grace-replay counter) change.
INSERT INTO identity.refresh_tokens (
    id, family_id, token_hash, generation,
    issued_at, expires_at, consumed_at, replaced_by_id,
    consumed_client_binding, grace_replays
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    consumed_at             = EXCLUDED.consumed_at,
    replaced_by_id          = EXCLUDED.replaced_by_id,
    consumed_client_binding = EXCLUDED.consumed_client_binding,
    grace_replays           = EXCLUDED.grace_replays;

-- name: GetRefreshTokenFamilyByID :one
SELECT id, person_id, tenant_id, device_label,
       created_at, last_used_at, revoked_at, revoke_reason
FROM   identity.refresh_token_families
WHERE  id = $1;

-- name: GetRefreshTokenFamilyByIDForUpdate :one
-- The aggregate-writer load (UpdateByID): takes the row lock so a rotation and a
-- revocation of the same family serialise. UpdateRefreshTokenFamily rewrites
-- revoked_at from the loaded snapshot, so an unlocked load let a rotation that
-- started before a password change write revoked_at = NULL back over it
-- (resurrecting the family). FOR NO KEY UPDATE: conflicts with other writers but
-- not with the FOR KEY SHARE the token rows' foreign key takes.
-- Lock order (identity): person row first, then family rows. Nothing that holds a
-- family lock takes a person lock.
SELECT id, person_id, tenant_id, device_label,
       created_at, last_used_at, revoked_at, revoke_reason
FROM   identity.refresh_token_families
WHERE  id = $1
FOR NO KEY UPDATE;

-- name: GetRefreshTokenByHash :one
-- Single indexed lookup by token hash — the rotation flow's entry point.
-- Returns the token row; caller resolves the family via family_id.
SELECT id, family_id, token_hash, generation,
       issued_at, expires_at, consumed_at, replaced_by_id,
       consumed_client_binding, grace_replays
FROM   identity.refresh_tokens
WHERE  token_hash = $1;

-- name: ListRefreshTokensInFamily :many
SELECT id, family_id, token_hash, generation,
       issued_at, expires_at, consumed_at, replaced_by_id,
       consumed_client_binding, grace_replays
FROM   identity.refresh_tokens
WHERE  family_id = $1
ORDER  BY generation;

-- name: ListActiveFamiliesForPerson :many
-- Lists every non-revoked refresh-token family for a Person across
-- tenants. Used by the post-security-event revocation cascade
-- (RevokeFamiliesOnSecurityChange) and the user "manage sessions" UI.
-- Cross-tenant query; non-RLS table per refresh_token canon.
SELECT id, person_id, tenant_id, device_label,
       created_at, last_used_at, revoked_at, revoke_reason
FROM   identity.refresh_token_families
WHERE  person_id  = $1
  AND  revoked_at IS NULL
ORDER  BY created_at;

-- name: ListActiveFamiliesForTenant :many
-- One bounded page (the caller revokes the page and asks again until it is empty)
-- of the non-revoked families bound to a tenant. Used by the tenant-suspension
-- cascade (RevokeFamiliesOnSecurityChange.HandleTenantSuspended). Cross-person,
-- non-RLS table per refresh_token canon. No index leads with tenant_id; a
-- suspension is a rare operator action and the partial index on active families
-- keeps the scan to the live rows.
SELECT id, person_id, tenant_id, device_label,
       created_at, last_used_at, revoked_at, revoke_reason
FROM   identity.refresh_token_families
WHERE  tenant_id  = $1
  AND  revoked_at IS NULL
ORDER  BY created_at
LIMIT  $2;

-- name: UpdateRefreshTokenFamily :exec
-- Persists family-level mutable state: last_used_at on every rotate;
-- revoked_at + revoke_reason on Revoke or reuse-detected.
UPDATE identity.refresh_token_families
SET    last_used_at  = $2,
       revoked_at    = $3,
       revoke_reason = $4
WHERE  id = $1;
