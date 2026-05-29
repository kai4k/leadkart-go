-- Platform module — PlatformLead queries. Per ADR 0059.

-- name: InsertPlatformLead :exec
INSERT INTO platform.platform_leads (
    id, source_contact_id,
    sold_to_tenant_id, sold_at, sold_to_membership_id, amount_paisa,
    contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
    has_drug_licence, has_gst, gst_number, gst_verified, has_pan, pan_number,
    business_type, medicine_system, product_ranges, dosage_forms,
    order_value, buy_timeline,
    verified_at, verified_by_membership_id, created_at
) VALUES (
    $1, $2,
    $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $20,
    $21, $22, $23, $24,
    $25, $26,
    $27, $28, $29
);

-- name: UpdatePlatformLead :exec
UPDATE platform.platform_leads SET
    sold_to_tenant_id     = $2,
    sold_at               = $3,
    sold_to_membership_id = $4,
    amount_paisa          = $5,
    gst_verified          = $6
WHERE id = $1;

-- name: GetPlatformLeadByID :one
SELECT id, source_contact_id,
       sold_to_tenant_id, sold_at, sold_to_membership_id, amount_paisa,
       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
       has_drug_licence, has_gst, gst_number, gst_verified, has_pan, pan_number,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       verified_at, verified_by_membership_id, created_at
FROM   platform.platform_leads
WHERE  id = $1;

-- name: MarketplaceBrowse :many
-- BRD §4.3 marketplace browse. Null-guarded optional filters: a NULL
-- arg means "don't filter on this column". Array filters use GIN `&&`
-- overlap; keyset cursor on (verified_at, id) DESC.
--
-- H12 SECURITY: the SELECT list DELIBERATELY OMITS PII columns
-- (email, mobile_e164, gst_number, pan_number, street). This is a
-- hard projection boundary per ADR 0059 — never add those columns.
-- Because the list is a strict subset, sqlc emits a custom
-- MarketplaceBrowseRow type (not the full PlatformPlatformLead model);
-- that's intentional and correct here.
SELECT id, source_contact_id,
       sold_to_tenant_id, sold_at, sold_to_membership_id, amount_paisa,
       contact_name, pincode, city, district, state_geo,
       has_drug_licence, has_gst, gst_verified, has_pan,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       verified_at, verified_by_membership_id, created_at
FROM   platform.platform_leads
WHERE  sold_to_tenant_id IS NULL
AND    (sqlc.narg('state')::text IS NULL          OR state_geo = sqlc.narg('state'))
AND    (sqlc.narg('city')::text IS NULL           OR city = sqlc.narg('city'))
AND    (sqlc.narg('district')::text IS NULL       OR district = sqlc.narg('district'))
AND    (sqlc.narg('pincode')::text IS NULL        OR pincode = sqlc.narg('pincode'))
AND    (sqlc.narg('business_type')::text IS NULL  OR business_type = sqlc.narg('business_type'))
AND    (sqlc.narg('medicine_system')::text IS NULL OR medicine_system = sqlc.narg('medicine_system'))
AND    (sqlc.narg('order_value')::text IS NULL    OR order_value = sqlc.narg('order_value'))
AND    (sqlc.narg('buy_timeline')::text IS NULL   OR buy_timeline = sqlc.narg('buy_timeline'))
AND    (sqlc.narg('has_drug_licence')::boolean IS NULL OR has_drug_licence = sqlc.narg('has_drug_licence'))
AND    (sqlc.narg('has_gst')::boolean IS NULL     OR has_gst = sqlc.narg('has_gst'))
AND    (sqlc.narg('gst_verified')::boolean IS NULL OR gst_verified = sqlc.narg('gst_verified'))
AND    (sqlc.narg('product_ranges')::text[] IS NULL OR product_ranges && sqlc.narg('product_ranges')::text[])
AND    (sqlc.narg('dosage_forms')::text[] IS NULL OR dosage_forms && sqlc.narg('dosage_forms')::text[])
AND    (sqlc.narg('cursor_verified_at')::timestamptz IS NULL
        OR (verified_at, id) < (sqlc.narg('cursor_verified_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER  BY verified_at DESC, id DESC
LIMIT  sqlc.arg('lim');
