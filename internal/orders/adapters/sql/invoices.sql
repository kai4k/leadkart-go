-- Orders module — Invoice queries (BRD §A-014). Append-only. Tenant scoping
-- via RLS.

-- name: InsertInvoice :exec
INSERT INTO orders.invoices (
    id, tenant_id, order_id,
    number_kind, number_financial_year, number_seq, number_display,
    line_items, tax_lines, subtotal_paise, tax_paise, grand_total_paise,
    issued_at, issued_by_membership_id
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11, $12,
    $13, $14
);

-- name: GetInvoiceByID :one
SELECT id, tenant_id, order_id,
       number_kind, number_financial_year, number_seq, number_display,
       line_items, tax_lines, subtotal_paise, tax_paise, grand_total_paise,
       issued_at, issued_by_membership_id
FROM   orders.invoices
WHERE  id = $1;

-- name: GetInvoiceByOrderID :one
SELECT id, tenant_id, order_id,
       number_kind, number_financial_year, number_seq, number_display,
       line_items, tax_lines, subtotal_paise, tax_paise, grand_total_paise,
       issued_at, issued_by_membership_id
FROM   orders.invoices
WHERE  order_id = $1;
