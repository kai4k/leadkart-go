-- Orders module — Quotation queries (ADR 0063). Tenant scoping via RLS (the
-- adapter binds app.tenant_id); WHERE clauses key on id only.

-- name: InsertQuotation :exec
INSERT INTO orders.quotations (
    id, tenant_id, customer_lead_id, state, revisions,
    approved_at, approved_by_membership_id,
    rejected_at, rejected_by_membership_id, rejection_reason,
    created_at, created_by_membership_id
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7,
    $8, $9, $10,
    $11, $12
);

-- name: GetQuotationByID :one
SELECT id, tenant_id, customer_lead_id, state, revisions,
       approved_at, approved_by_membership_id,
       rejected_at, rejected_by_membership_id, rejection_reason,
       created_at, created_by_membership_id
FROM   orders.quotations
WHERE  id = $1;

-- name: GetQuotationByIDForUpdate :one
-- Row-locks the quotation for the UpdateFn transaction so concurrent revise /
-- approve / reject calls serialise.
SELECT id, tenant_id, customer_lead_id, state, revisions,
       approved_at, approved_by_membership_id,
       rejected_at, rejected_by_membership_id, rejection_reason,
       created_at, created_by_membership_id
FROM   orders.quotations
WHERE  id = $1
FOR UPDATE;

-- name: UpdateQuotation :exec
-- Persists the mutable columns. Identity + creation facts never change.
UPDATE orders.quotations SET
    state                     = $2,
    revisions                 = $3,
    approved_at               = $4,
    approved_by_membership_id = $5,
    rejected_at               = $6,
    rejected_by_membership_id = $7,
    rejection_reason          = $8
WHERE id = $1;
