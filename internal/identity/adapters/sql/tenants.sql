-- Tenant queries — identity.tenants is non-RLS (each row IS a tenant).
-- Lookups are unscoped; tenant resolution is the load-bearing isolation
-- elsewhere (membership lookup binds the tenant_id GUC for RLS).

-- name: InsertTenant :exec
INSERT INTO identity.tenants (
    id, slug, legal_name, display_name, admin_email, status, created_at,
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
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10,
    $11, $12, $13, $14, $15, $16, $17,
    $18, $19, $20, $21, $22, $23, $24,
    $25, $26, $27, $28,
    $29, $30, $31
);

-- name: GetTenantByID :one
SELECT id, slug, legal_name, display_name, admin_email, status,
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
SELECT id, slug, legal_name, display_name, admin_email, status,
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
SELECT id, slug, legal_name, display_name, admin_email, status,
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
UPDATE identity.tenants
SET    legal_name                  = $2,
       display_name                 = $3,
       admin_email                  = $4,
       status                       = $5,
       activated_at                 = $6,
       suspended_at                 = $7,
       gst_number                   = $8,
       pan_number                   = $9,
       drug_licence_number          = $10,
       admin_phone                  = $11,
       admin_address_street         = $12,
       admin_address_city           = $13,
       admin_address_district       = $14,
       admin_address_state          = $15,
       admin_address_state_code     = $16,
       admin_address_pincode        = $17,
       password_min_length          = $18,
       password_require_uppercase   = $19,
       password_require_lowercase   = $20,
       password_require_digit       = $21,
       password_require_symbol      = $22,
       password_max_failed_attempts = $23,
       password_lockout_minutes     = $24,
       locale                       = $25,
       time_zone                    = $26,
       date_format                  = $27,
       currency                     = $28,
       deletion_scheduled_at        = $29,
       deletion_reason              = $30,
       hard_deleted_at              = $31
WHERE  id = $1;

-- name: HardDeleteTenant :exec
-- Operator-only physical row delete. Per data-retention.md
-- "Tenant deletion saga": HardDelete is the final step after the
-- 30-day grace window expires AND every module has acknowledged the
-- deletion event. The aggregate's HardDelete() method gates this on
-- StatusPendingDeletion + grace expiry; the SQL is unconditional.
DELETE FROM identity.tenants WHERE id = $1;
