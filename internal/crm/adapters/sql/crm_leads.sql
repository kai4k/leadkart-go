-- CrmLead queries — crm.crm_leads is RLS+FORCE per ADR 0006 +
-- migration 20260602000001. Reads/writes flow through RLS: only the
-- current tenant's rows are visible unless app.is_platform=true.
--
-- Slice 1 surfaces:
--   InsertCrmLead          → repo Add path (subscriber + future manual import)
--   GetCrmLeadByID         → repo GetByID + UpdateByID load path
--   GetCrmLeadByPurchaseID → subscriber idempotency check (ADR 0060
--                             natural-key idempotency)
--   UpdateCrmLead          → repo UpdateByID persist path; writes the
--                             mutable columns (stage, temperature,
--                             assignee, lifecycle terminals)
--   ListCrmLeadsPage       → cursor-paginated list per ADR 0038. Filter
--                             columns merged inline; null/empty filters
--                             short-circuit via IS NULL checks.

-- name: InsertCrmLead :exec
INSERT INTO crm.crm_leads (
    id, tenant_id, source_purchase_id, source_platform_lead_id,
    stage, temperature,
    contact_name, phone_e164, city, district, state, pincode,
    business_type, medicine_system, order_value, buy_timeline,
    has_drug_licence, has_gst, gst_verified,
    product_ranges, dosage_forms, extra_profile,
    assignee_membership_id, assigned_at,
    converted_at, converted_by_membership_id,
    lost_at, lost_by_membership_id, lost_reason,
    created_at, created_by_membership_id
) VALUES (
    $1, $2, $3, $4,
    $5, $6,
    $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16,
    $17, $18, $19,
    $20, $21, $22,
    $23, $24,
    $25, $26,
    $27, $28, $29,
    $30, $31
);

-- name: GetCrmLeadByID :one
SELECT id, tenant_id, source_purchase_id, source_platform_lead_id,
       stage, temperature,
       contact_name, phone_e164, city, district, state, pincode,
       business_type, medicine_system, order_value, buy_timeline,
       has_drug_licence, has_gst, gst_verified,
       product_ranges, dosage_forms, extra_profile,
       assignee_membership_id, assigned_at,
       converted_at, converted_by_membership_id,
       lost_at, lost_by_membership_id, lost_reason,
       created_at, created_by_membership_id
FROM   crm.crm_leads
WHERE  id = $1;

-- name: GetCrmLeadByPurchaseID :one
-- Subscriber idempotency lookup. NULL inputs are not valid here — the
-- subscriber's command short-circuits before the call when PurchaseID
-- is empty.
SELECT id, tenant_id, source_purchase_id, source_platform_lead_id,
       stage, temperature,
       contact_name, phone_e164, city, district, state, pincode,
       business_type, medicine_system, order_value, buy_timeline,
       has_drug_licence, has_gst, gst_verified,
       product_ranges, dosage_forms, extra_profile,
       assignee_membership_id, assigned_at,
       converted_at, converted_by_membership_id,
       lost_at, lost_by_membership_id, lost_reason,
       created_at, created_by_membership_id
FROM   crm.crm_leads
WHERE  source_purchase_id = $1;

-- name: UpdateCrmLead :exec
-- Persists the mutable CrmLead state. tenant_id + source_* + created_at
-- + created_by_membership_id are aggregate-immutable; this query
-- intentionally does NOT write them.
UPDATE crm.crm_leads
SET    stage                      = $2,
       temperature                = $3,
       assignee_membership_id     = $4,
       assigned_at                = $5,
       converted_at               = $6,
       converted_by_membership_id = $7,
       lost_at                    = $8,
       lost_by_membership_id      = $9,
       lost_reason                = $10,
       extra_profile              = $11,
       contact_name               = $12,
       phone_e164                 = $13,
       city                       = $14,
       district                   = $15,
       state                      = $16,
       pincode                    = $17,
       business_type              = $18,
       medicine_system            = $19,
       order_value                = $20,
       buy_timeline               = $21,
       has_drug_licence           = $22,
       has_gst                    = $23,
       gst_verified               = $24,
       product_ranges             = $25,
       dosage_forms               = $26
WHERE  id = $1;

-- name: ListCrmLeadsPage :many
-- Cursor (keyset) pagination on (created_at, id) DESC per ADR 0038.
-- Composite filter columns supplied as nullable params — pass NULL /
-- empty to disable a given filter.
--
-- LIMIT is $page_size + 1 (peek-one-extra) — adapter strips the extra
-- row + sets HasMore based on the returned row count.
--
-- pg_trgm name search uses ILIKE so the GIN trgm index lights up
-- naturally; the adapter wraps the user-supplied query in `%pat%`.
SELECT id, tenant_id, source_purchase_id, source_platform_lead_id,
       stage, temperature,
       contact_name, phone_e164, city, district, state, pincode,
       business_type, medicine_system, order_value, buy_timeline,
       has_drug_licence, has_gst, gst_verified,
       product_ranges, dosage_forms, extra_profile,
       assignee_membership_id, assigned_at,
       converted_at, converted_by_membership_id,
       lost_at, lost_by_membership_id, lost_reason,
       created_at, created_by_membership_id
FROM   crm.crm_leads
WHERE  tenant_id = $1
  AND  (sqlc.narg('stage')::text IS NULL OR stage = sqlc.narg('stage')::text)
  AND  (sqlc.narg('temperature')::text IS NULL OR temperature = sqlc.narg('temperature')::text)
  AND  (sqlc.narg('assignee')::uuid IS NULL OR assignee_membership_id = sqlc.narg('assignee')::uuid)
  AND  (sqlc.narg('self_assignee')::uuid IS NULL OR assignee_membership_id = sqlc.narg('self_assignee')::uuid)
  AND  (sqlc.narg('city')::text IS NULL OR city = sqlc.narg('city')::text)
  AND  (sqlc.narg('pincode')::text IS NULL OR pincode = sqlc.narg('pincode')::text)
  AND  (sqlc.narg('business_type')::text IS NULL OR business_type = sqlc.narg('business_type')::text)
  AND  (sqlc.narg('medicine_system')::text IS NULL OR medicine_system = sqlc.narg('medicine_system')::text)
  AND  (sqlc.narg('product_ranges')::text[] IS NULL OR product_ranges @> sqlc.narg('product_ranges')::text[])
  AND  (sqlc.narg('dosage_forms')::text[] IS NULL OR dosage_forms @> sqlc.narg('dosage_forms')::text[])
  AND  (sqlc.narg('name_query')::text IS NULL OR contact_name ILIKE sqlc.narg('name_query')::text)
  AND  (sqlc.narg('cursor_created_at')::timestamptz IS NULL OR
        (created_at, id) < (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER  BY created_at DESC, id DESC
LIMIT  sqlc.arg('page_size')::int;
