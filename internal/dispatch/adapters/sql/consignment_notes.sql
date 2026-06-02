-- Dispatch module — ConsignmentNote queries. Per ADR 0063.
-- Tenant scoping is enforced by RLS (the adapter binds app.tenant_id via the
-- transactor); WHERE clauses key on id / order_id only.

-- name: InsertConsignmentNote :exec
INSERT INTO dispatch.consignment_notes (
    id, tenant_id, order_id, status, carrier_name, docket_number,
    box_count, weight_grams, expected_delivery_at,
    dispatched_at, in_transit_at, delivered_at, failed_at, failure_reason,
    created_at, created_by_membership_id
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9,
    $10, $11, $12, $13, $14,
    $15, $16
);

-- name: GetConsignmentNoteByID :one
SELECT id, tenant_id, order_id, status, carrier_name, docket_number,
       box_count, weight_grams, expected_delivery_at,
       dispatched_at, in_transit_at, delivered_at, failed_at, failure_reason,
       created_at, created_by_membership_id
FROM   dispatch.consignment_notes
WHERE  id = $1;

-- name: GetConsignmentNoteByOrderID :one
SELECT id, tenant_id, order_id, status, carrier_name, docket_number,
       box_count, weight_grams, expected_delivery_at,
       dispatched_at, in_transit_at, delivered_at, failed_at, failure_reason,
       created_at, created_by_membership_id
FROM   dispatch.consignment_notes
WHERE  order_id = $1;

-- name: GetConsignmentNoteByIDForUpdate :one
-- Row-locks the note for the UpdateFn transaction so concurrent status
-- transitions serialise.
SELECT id, tenant_id, order_id, status, carrier_name, docket_number,
       box_count, weight_grams, expected_delivery_at,
       dispatched_at, in_transit_at, delivered_at, failed_at, failure_reason,
       created_at, created_by_membership_id
FROM   dispatch.consignment_notes
WHERE  id = $1
FOR UPDATE;

-- name: UpdateConsignmentNoteState :exec
-- Persists the mutable lifecycle columns. The immutable shipment facts
-- (carrier, box_count, weight, expected_delivery, created_*) never change.
UPDATE dispatch.consignment_notes SET
    status         = $2,
    docket_number  = $3,
    dispatched_at  = $4,
    in_transit_at  = $5,
    delivered_at   = $6,
    failed_at      = $7,
    failure_reason = $8
WHERE id = $1;
