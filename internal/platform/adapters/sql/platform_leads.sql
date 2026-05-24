-- Platform module — PlatformLead queries. Per ADR 0059.
--
-- Static reads only — the marketplace browse with dynamic filters is
-- assembled via squirrel directly in the adapter (sqlc can't express
-- the optional WHERE clauses on text[] GIN columns cleanly).

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
