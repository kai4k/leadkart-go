-- Alert-emission dedup queries — feed the ExpiryScanJob +
-- ReorderScanJob workers. PK on (tenant_id, kind, subject_id,
-- emitted_date) makes second-run-same-day a no-op via INSERT ON
-- CONFLICT DO NOTHING.

-- name: InsertAlertEmission :execrows
-- Returns rows-affected — 0 when the row already existed (deduped);
-- 1 when freshly inserted (caller publishes the integration event).
INSERT INTO inventory.alert_emissions (
    tenant_id, kind, subject_id, emitted_date
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (tenant_id, kind, subject_id, emitted_date) DO NOTHING;

-- name: ListTenantsWithProducts :many
-- Distinct tenant_ids carrying at least one LIVE product. Drives the
-- per-tenant scan loop in ExpiryScanJob + ReorderScanJob — runs
-- under platform-scope (no GUC binding) so the worker sees all
-- tenants. Result set bounded by tenant count (low-thousands at
-- v0.2 scale).
SELECT DISTINCT tenant_id
FROM   inventory.products
WHERE  NOT is_deleted
ORDER  BY tenant_id ASC;
