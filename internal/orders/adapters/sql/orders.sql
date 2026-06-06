-- Orders module — Order queries (ADR 0063). Tenant scoping via RLS.

-- name: InsertOrder :exec
INSERT INTO orders.orders (
    id, tenant_id, approved_quotation_id, customer_lead_id, state,
    confirmed_items, subtotal_paise, tax_paise, grand_total_paise,
    invoice_id, consignment_note_id,
    confirmed_at, packed_at, invoiced_at, dispatched_at, delivered_at,
    completed_at, cancelled_at, cancellation_reason,
    created_at, created_by_membership_id
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11,
    $12, $13, $14, $15, $16,
    $17, $18, $19,
    $20, $21
);

-- name: GetOrderByID :one
SELECT id, tenant_id, approved_quotation_id, customer_lead_id, state,
       confirmed_items, subtotal_paise, tax_paise, grand_total_paise,
       invoice_id, consignment_note_id,
       confirmed_at, packed_at, invoiced_at, dispatched_at, delivered_at,
       completed_at, cancelled_at, cancellation_reason,
       created_at, created_by_membership_id
FROM   orders.orders
WHERE  id = $1;

-- name: GetOrderByIDForUpdate :one
-- Row-locks the order for the UpdateFn transaction so concurrent lifecycle
-- transitions serialise.
SELECT id, tenant_id, approved_quotation_id, customer_lead_id, state,
       confirmed_items, subtotal_paise, tax_paise, grand_total_paise,
       invoice_id, consignment_note_id,
       confirmed_at, packed_at, invoiced_at, dispatched_at, delivered_at,
       completed_at, cancelled_at, cancellation_reason,
       created_at, created_by_membership_id
FROM   orders.orders
WHERE  id = $1
FOR UPDATE;

-- name: UpdateOrder :exec
-- Persists the mutable lifecycle columns. Confirmed items + money totals +
-- creation facts are snapshotted at creation and never change.
UPDATE orders.orders SET
    state               = $2,
    invoice_id          = $3,
    consignment_note_id = $4,
    confirmed_at        = $5,
    packed_at           = $6,
    invoiced_at         = $7,
    dispatched_at       = $8,
    delivered_at        = $9,
    completed_at        = $10,
    cancelled_at        = $11,
    cancellation_reason = $12
WHERE id = $1;
