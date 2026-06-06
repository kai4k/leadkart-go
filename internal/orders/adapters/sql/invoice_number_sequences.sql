-- Gapless invoice-number allocation (ADR 0063 §3). The allocator runs both
-- statements inside the order's UoW tx: a rollback rolls back the increment,
-- so the per-(tenant, FY, kind) sequence never gaps.

-- name: EnsureInvoiceNumberSequence :exec
INSERT INTO orders.invoice_number_sequences (tenant_id, financial_year, kind, last_used)
VALUES (sqlc.arg('tenant_id'), sqlc.arg('financial_year'), sqlc.arg('kind'), 0)
ON CONFLICT (tenant_id, financial_year, kind) DO NOTHING;

-- name: AllocateInvoiceNumber :one
UPDATE orders.invoice_number_sequences
SET    last_used = last_used + 1
WHERE  tenant_id = sqlc.arg('tenant_id')
  AND  financial_year = sqlc.arg('financial_year')
  AND  kind = sqlc.arg('kind')
RETURNING last_used;
