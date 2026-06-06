-- Orders module — CreditNote queries (BRD §A-014). Append-only. Tenant
-- scoping via RLS.

-- name: InsertCreditNote :exec
INSERT INTO orders.credit_notes (
    id, tenant_id, invoice_id,
    number_kind, number_financial_year, number_seq, number_display,
    reason, amount_paise,
    issued_at, issued_by_membership_id
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9,
    $10, $11
);

-- name: GetCreditNoteByID :one
SELECT id, tenant_id, invoice_id,
       number_kind, number_financial_year, number_seq, number_display,
       reason, amount_paise,
       issued_at, issued_by_membership_id
FROM   orders.credit_notes
WHERE  id = $1;

-- name: ListCreditNotesByInvoice :many
SELECT id, tenant_id, invoice_id,
       number_kind, number_financial_year, number_seq, number_display,
       reason, amount_paise,
       issued_at, issued_by_membership_id
FROM   orders.credit_notes
WHERE  invoice_id = $1
ORDER  BY issued_at, id;
