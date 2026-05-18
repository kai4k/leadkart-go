-- Tenant queries — identity.tenants is non-RLS (each row IS a tenant).
-- Lookups are unscoped; tenant resolution is the load-bearing isolation
-- elsewhere (membership lookup binds the tenant_id GUC for RLS).

-- name: InsertTenant :exec
INSERT INTO identity.tenants (
    id, slug, legal_name, display_name, status, created_at,
    gst_number, pan_number, drug_licence_number,
    admin_phone, admin_address_street, admin_address_city,
    admin_address_district, admin_address_state, admin_address_state_code,
    admin_address_pincode,
    password_min_length, password_require_uppercase, password_require_lowercase,
    password_require_digit, password_require_symbol,
    password_max_failed_attempts, password_lockout_minutes,
    locale, time_zone, date_format, currency,
    deletion_scheduled_at, deletion_reason, hard_deleted_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9,
    $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20, $21, $22, $23,
    $24, $25, $26, $27,
    $28, $29, $30
);

-- name: GetTenantByID :one
SELECT id, slug, legal_name, display_name, status,
       created_at, activated_at, suspended_at,
       gst_number, pan_number, drug_licence_number,
       admin_phone, admin_address_street, admin_address_city,
       admin_address_district, admin_address_state, admin_address_state_code,
       admin_address_pincode,
       password_min_length, password_require_uppercase, password_require_lowercase,
       password_require_digit, password_require_symbol,
       password_max_failed_attempts, password_lockout_minutes,
       locale, time_zone, date_format, currency,
       deletion_scheduled_at, deletion_reason, hard_deleted_at
FROM   identity.tenants
WHERE  id = $1;

-- name: GetTenantBySlug :one
SELECT id, slug, legal_name, display_name, status,
       created_at, activated_at, suspended_at,
       gst_number, pan_number, drug_licence_number,
       admin_phone, admin_address_street, admin_address_city,
       admin_address_district, admin_address_state, admin_address_state_code,
       admin_address_pincode,
       password_min_length, password_require_uppercase, password_require_lowercase,
       password_require_digit, password_require_symbol,
       password_max_failed_attempts, password_lockout_minutes,
       locale, time_zone, date_format, currency,
       deletion_scheduled_at, deletion_reason, hard_deleted_at
FROM   identity.tenants
WHERE  slug = $1;

-- name: ListAllTenants :many
-- Cross-tenant listing — Platform-operator path only. The aggregate
-- table is non-RLS, so this returns every row regardless of the
-- caller's tenant context. The HTTP layer gates on RequirePlatform
-- before dispatching here.
SELECT id, slug, legal_name, display_name, status,
       created_at, activated_at, suspended_at,
       gst_number, pan_number, drug_licence_number,
       admin_phone, admin_address_street, admin_address_city,
       admin_address_district, admin_address_state, admin_address_state_code,
       admin_address_pincode,
       password_min_length, password_require_uppercase, password_require_lowercase,
       password_require_digit, password_require_symbol,
       password_max_failed_attempts, password_lockout_minutes,
       locale, time_zone, date_format, currency,
       deletion_scheduled_at, deletion_reason, hard_deleted_at
FROM   identity.tenants
ORDER  BY created_at DESC;

-- name: UpdateTenant :exec
-- General-purpose update covering UpdateProfile + UpdateStatutory +
-- UpdateAdminContact + UpdateSettings + UpdateDisplayPreferences +
-- Activate + Suspend + MarkForDeletion + RestoreFromDeletion +
-- HardDelete. Repository writes whatever the aggregate currently says.
--
-- admin_email removed in migration 20260507000008 — current admin
-- email is derived at read-time via JOIN through CompanyOwner role.
UPDATE identity.tenants
SET    legal_name                  = $2,
       display_name                 = $3,
       status                       = $4,
       activated_at                 = $5,
       suspended_at                 = $6,
       gst_number                   = $7,
       pan_number                   = $8,
       drug_licence_number          = $9,
       admin_phone                  = $10,
       admin_address_street         = $11,
       admin_address_city           = $12,
       admin_address_district       = $13,
       admin_address_state          = $14,
       admin_address_state_code     = $15,
       admin_address_pincode        = $16,
       password_min_length          = $17,
       password_require_uppercase   = $18,
       password_require_lowercase   = $19,
       password_require_digit       = $20,
       password_require_symbol      = $21,
       password_max_failed_attempts = $22,
       password_lockout_minutes     = $23,
       locale                       = $24,
       time_zone                    = $25,
       date_format                  = $26,
       currency                     = $27,
       deletion_scheduled_at        = $28,
       deletion_reason              = $29,
       hard_deleted_at              = $30
WHERE  id = $1;

-- name: HardDeleteTenant :exec
-- Operator-only physical row delete. Per data-retention.md
-- "Tenant deletion saga": HardDelete is the final step after the
-- 30-day grace window expires AND every module has acknowledged the
-- deletion event. The aggregate's HardDelete() method gates this on
-- StatusPendingDeletion + grace expiry; the SQL is unconditional.
DELETE FROM identity.tenants WHERE id = $1;


-- name: SearchTenantsByText :many
-- Cross-tenant tenants search per ADR 0040 (pg_trgm). Backed by
-- idx_tenants_search_trgm (GIN over lower(slug||' '||legal_name||
-- ' '||display_name)).
--
-- Tenants is non-RLS so this runs in any scope; the HTTP layer
-- gates on RequirePlatform anyway. similarity() ranking surfaces
-- closer matches first. Caller MUST bound query length at the
-- boundary (2-100 chars).
SELECT id, slug, legal_name, display_name, status, created_at,
       similarity(
           lower(slug) || ' ' || lower(legal_name) || ' ' || lower(display_name),
           lower($1)
       ) AS rank
FROM   identity.tenants
WHERE  status != 'hard_deleted'
AND    (lower(slug) || ' ' || lower(legal_name) || ' ' || lower(display_name))
       ILIKE '%' || lower($1) || '%'
ORDER  BY rank DESC, id DESC
LIMIT  $2;
