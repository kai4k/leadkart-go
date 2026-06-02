-- Platform module — PlatformLead queries. Per ADR 0059 + ADR 0065 (multi-buyer).

-- name: InsertPlatformLead :exec
INSERT INTO platform.platform_leads (
    id, source_contact_id, tier, sale_limit,
    contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
    has_drug_licence, has_gst, gst_number, gst_verified, has_pan, pan_number,
    business_type, medicine_system, product_ranges, dosage_forms,
    order_value, buy_timeline,
    verified_at, verified_by_membership_id, created_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18,
    $19, $20, $21, $22,
    $23, $24,
    $25, $26, $27
);

-- name: UpdatePlatformLeadGstVerified :exec
-- The only post-creation mutation of the lead row itself; a purchase is an
-- INSERT into lead_purchases, not an UPDATE here.
UPDATE platform.platform_leads SET
    gst_verified = $2
WHERE id = $1;

-- name: GetPlatformLeadByID :one
SELECT id, source_contact_id, tier, sale_limit,
       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
       has_drug_licence, has_gst, gst_number, gst_verified, has_pan, pan_number,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       verified_at, verified_by_membership_id, created_at
FROM   platform.platform_leads
WHERE  id = $1;

-- name: GetPlatformLeadByIDForUpdate :one
-- Row-locks the lead for the purchase tx so concurrent purchases of the same
-- lead serialise — the sale-limit count check is then race-free (ADR 0065).
SELECT id, source_contact_id, tier, sale_limit,
       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
       has_drug_licence, has_gst, gst_number, gst_verified, has_pan, pan_number,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       verified_at, verified_by_membership_id, created_at
FROM   platform.platform_leads
WHERE  id = $1
FOR UPDATE;

-- name: MarketplaceBrowse :many
-- BRD §4.3 marketplace browse. Null-guarded optional filters: a NULL
-- arg means "don't filter on this column". Array filters use GIN `&&`
-- overlap; keyset cursor on (verified_at, id) DESC.
--
-- Availability (ADR 0065): a lead is browsable while its purchase count is
-- below the effective sale limit (per-lead override, else tier default). A
-- tenant that already bought the lead is filtered defensively at purchase time
-- (RecordPurchase -> ErrAlreadyPurchased), not here.
--
-- H12 SECURITY: the SELECT list DELIBERATELY OMITS PII columns
-- (email, mobile_e164, gst_number, pan_number, street). This is a
-- hard projection boundary per ADR 0059 — never add those columns.
-- Because the list is a strict subset, sqlc emits a custom
-- MarketplaceBrowseRow type (not the full PlatformPlatformLead model);
-- that's intentional and correct here.
SELECT pl.id, pl.source_contact_id, pl.tier, pl.sale_limit,
       pl.contact_name, pl.pincode, pl.city, pl.district, pl.state_geo,
       pl.has_drug_licence, pl.has_gst, pl.gst_verified, pl.has_pan,
       pl.business_type, pl.medicine_system, pl.product_ranges, pl.dosage_forms,
       pl.order_value, pl.buy_timeline,
       pl.verified_at, pl.verified_by_membership_id, pl.created_at
FROM   platform.platform_leads pl
WHERE  (
           SELECT count(*) FROM platform.lead_purchases lp
           WHERE lp.lead_id = pl.id
       ) < COALESCE(
           pl.sale_limit,
           (SELECT t.default_sale_limit FROM platform.lead_tiers t WHERE t.code = pl.tier)
       )
AND    (sqlc.narg('state')::text IS NULL          OR pl.state_geo = sqlc.narg('state'))
AND    (sqlc.narg('city')::text IS NULL           OR pl.city = sqlc.narg('city'))
AND    (sqlc.narg('district')::text IS NULL       OR pl.district = sqlc.narg('district'))
AND    (sqlc.narg('pincode')::text IS NULL        OR pl.pincode = sqlc.narg('pincode'))
AND    (sqlc.narg('business_type')::text IS NULL  OR pl.business_type = sqlc.narg('business_type'))
AND    (sqlc.narg('medicine_system')::text IS NULL OR pl.medicine_system = sqlc.narg('medicine_system'))
AND    (sqlc.narg('order_value')::text IS NULL    OR pl.order_value = sqlc.narg('order_value'))
AND    (sqlc.narg('buy_timeline')::text IS NULL   OR pl.buy_timeline = sqlc.narg('buy_timeline'))
AND    (sqlc.narg('has_drug_licence')::boolean IS NULL OR pl.has_drug_licence = sqlc.narg('has_drug_licence'))
AND    (sqlc.narg('has_gst')::boolean IS NULL     OR pl.has_gst = sqlc.narg('has_gst'))
AND    (sqlc.narg('gst_verified')::boolean IS NULL OR pl.gst_verified = sqlc.narg('gst_verified'))
AND    (sqlc.narg('product_ranges')::text[] IS NULL OR pl.product_ranges && sqlc.narg('product_ranges')::text[])
AND    (sqlc.narg('dosage_forms')::text[] IS NULL OR pl.dosage_forms && sqlc.narg('dosage_forms')::text[])
AND    (sqlc.narg('cursor_verified_at')::timestamptz IS NULL
        OR (pl.verified_at, pl.id) < (sqlc.narg('cursor_verified_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER  BY pl.verified_at DESC, pl.id DESC
LIMIT  sqlc.arg('lim');
