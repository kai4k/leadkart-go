-- Audit-log read queries — buildingblocks.audit_log_entry.
--
-- The table is auto-written per command by the Watermill
-- AuditLoggingMiddleware (per ADR 0027 outbox-doubles-as-audit + the
-- additive buildingblocks audit table from migration
-- 20260507000001). Reads here are operator-facing — keyset paginated
-- per ADR 0038 + filtered by tenant_id / user_id / action.
--
-- Cross-tenant queries run under TxScopePlatform (the table has no
-- RLS); per-tenant queries also run under platform scope so the
-- handler explicitly authorises before dispatching.

-- name: ListAuditEventsByTenantPage :many
-- Keyset-paginated tenant-scoped events. Backed by
-- idx_audit_log_entry_tenant_occurred (tenant_id, occurred_at_utc DESC)
-- WHERE tenant_id IS NOT NULL.
--
-- Cursor predicate: (occurred_at_utc, id) < ($2, $3) for tuple tie-
-- break. First page sentinel: future timestamp + max UUID.
SELECT id, action, user_id, tenant_id, correlation_id,
       occurred_at_utc, duration_ms, succeeded, failure_reason, payload,
       act_operator_id, act_session_id, act_reason
FROM   buildingblocks.audit_log_entry
WHERE  tenant_id = $1
AND    (occurred_at_utc, id) < (sqlc.arg(before_occurred)::timestamptz, sqlc.arg(before_id)::uuid)
ORDER  BY occurred_at_utc DESC, id DESC
LIMIT  sqlc.arg('limit');

-- name: ListAuditEventsByUserPage :many
-- Keyset-paginated user-scoped events. Backed by
-- idx_audit_log_entry_user_occurred (user_id, occurred_at_utc DESC)
-- WHERE user_id IS NOT NULL.
SELECT id, action, user_id, tenant_id, correlation_id,
       occurred_at_utc, duration_ms, succeeded, failure_reason, payload,
       act_operator_id, act_session_id, act_reason
FROM   buildingblocks.audit_log_entry
WHERE  user_id = $1
AND    (occurred_at_utc, id) < (sqlc.arg(before_occurred)::timestamptz, sqlc.arg(before_id)::uuid)
ORDER  BY occurred_at_utc DESC, id DESC
LIMIT  sqlc.arg('limit');
