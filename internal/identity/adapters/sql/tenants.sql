-- Tenant queries — identity.tenants is non-RLS (each row IS a tenant).
-- Lookups are unscoped; tenant resolution is the load-bearing isolation
-- elsewhere (membership lookup binds the tenant_id GUC for RLS).

-- name: InsertTenant :exec
INSERT INTO identity.tenants (
    id, slug, legal_name, display_name, admin_email, status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetTenantByID :one
SELECT id, slug, legal_name, display_name, admin_email, status,
       created_at, activated_at, suspended_at
FROM   identity.tenants
WHERE  id = $1;

-- name: GetTenantBySlug :one
SELECT id, slug, legal_name, display_name, admin_email, status,
       created_at, activated_at, suspended_at
FROM   identity.tenants
WHERE  slug = $1;

-- name: UpdateTenantStatus :exec
UPDATE identity.tenants
SET    status       = $2,
       activated_at = $3,
       suspended_at = $4
WHERE  id = $1;
