-- Orders module — Payment queries (BRD §6.4). Append-only. Tenant scoping
-- via RLS.

-- name: InsertPayment :exec
INSERT INTO orders.payments (
    id, tenant_id, order_id, kind, method, amount_paise,
    external_reference, notes, received_at, recorded_at, recorded_by_membership_id
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11
);

-- name: GetPaymentByID :one
SELECT id, tenant_id, order_id, kind, method, amount_paise,
       external_reference, notes, received_at, recorded_at, recorded_by_membership_id
FROM   orders.payments
WHERE  id = $1;

-- name: ListPaymentsByOrder :many
SELECT id, tenant_id, order_id, kind, method, amount_paise,
       external_reference, notes, received_at, recorded_at, recorded_by_membership_id
FROM   orders.payments
WHERE  order_id = $1
ORDER  BY received_at, id;
