-- Platform module — UnverifiedContact queries. Per ADR 0059.

-- name: InsertUnverifiedContact :exec
INSERT INTO platform.unverified_contacts (
    id, state, rejection_reason,
    busy_callback_at, busy_callback_end_at, platform_lead_id,
    contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
    has_drug_licence, has_gst, gst_number, has_pan, pan_number,
    business_type, medicine_system, product_ranges, dosage_forms,
    order_value, buy_timeline,
    created_at, created_by_membership_id,
    verified_at, verified_by_membership_id,
    rejected_at, rejected_by_membership_id
) VALUES (
    $1, $2, $3,
    $4, $5, $6,
    $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19,
    $20, $21, $22, $23,
    $24, $25,
    $26, $27,
    $28, $29,
    $30, $31
);

-- name: UpdateUnverifiedContact :exec
UPDATE platform.unverified_contacts SET
    state                     = $2,
    rejection_reason          = $3,
    busy_callback_at          = $4,
    busy_callback_end_at      = $5,
    platform_lead_id          = $6,
    verified_at               = $7,
    verified_by_membership_id = $8,
    rejected_at               = $9,
    rejected_by_membership_id = $10
WHERE id = $1;

-- name: GetUnverifiedContactByID :one
SELECT id, state, rejection_reason,
       busy_callback_at, busy_callback_end_at, platform_lead_id,
       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
       has_drug_licence, has_gst, gst_number, has_pan, pan_number,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       created_at, created_by_membership_id,
       verified_at, verified_by_membership_id,
       rejected_at, rejected_by_membership_id
FROM   platform.unverified_contacts
WHERE  id = $1;

-- name: ListUnverifiedContactsPage :many
-- Keyset pagination on (created_at, id) DESC. State filter optional:
-- caller passes empty string to skip the state predicate.
--
-- Caller passes pageSize+1 to detect "has next page" per ADR 0038.
-- The cursor predicate is gated by `cursor_at` non-NULL so the first
-- page (cursor zero) skips the (created_at,id) < (...) comparison.
SELECT id, state, rejection_reason,
       busy_callback_at, busy_callback_end_at, platform_lead_id,
       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
       has_drug_licence, has_gst, gst_number, has_pan, pan_number,
       business_type, medicine_system, product_ranges, dosage_forms,
       order_value, buy_timeline,
       created_at, created_by_membership_id,
       verified_at, verified_by_membership_id,
       rejected_at, rejected_by_membership_id
FROM   platform.unverified_contacts
WHERE  (sqlc.arg(state_filter)::text = '' OR state = sqlc.arg(state_filter)::text)
  AND  (sqlc.arg(cursor_at)::timestamptz IS NULL
        OR (created_at, id) < (sqlc.arg(cursor_at)::timestamptz, sqlc.arg(cursor_id)::uuid))
ORDER  BY created_at DESC, id DESC
LIMIT  sqlc.arg(page_size);
