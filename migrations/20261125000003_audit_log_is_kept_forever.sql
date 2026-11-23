-- Owner decision 2026-11-23: "We never delete any data. We keep it all." (ADR 0071)
--
-- The audit trail has no age limit. The daily audit purge job is removed from the
-- worker, and this takes the last way to delete an audit row away from the
-- application role: DELETE on common.audit_log_entry is revoked, so the table is
-- append-only for the application (SELECT, INSERT). Nothing in the application can
-- delete a row of it, scheduled or not. The migration owner is unchanged.
--
-- No row is touched.

-- +goose Up
-- +goose StatementBegin

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        REVOKE DELETE ON common.audit_log_entry FROM leadkart_app;
    END IF;
END
$$;

COMMENT ON TABLE common.audit_log_entry IS
    'Auto-written per command via Watermill AuditLoggingMiddleware. Append-only and kept forever (ADR 0071): the application role holds SELECT and INSERT only.';
COMMENT ON TABLE common.admin_impersonation_audit IS
    'Per-request operator impersonation activity. Indexed on operator + target tenant for forensic queries. Append-only and kept forever (ADR 0071).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        GRANT DELETE ON common.audit_log_entry TO leadkart_app;
    END IF;
END
$$;

COMMENT ON TABLE common.audit_log_entry IS
    'Auto-written per command via Watermill AuditLoggingMiddleware. 7-year retention; daily purge.';
COMMENT ON TABLE common.admin_impersonation_audit IS
    'Per-request operator impersonation activity. Indexed on operator + target tenant for forensic queries. 7-year retention per SOC2 CC4.1 / DPDP §12.';

-- +goose StatementEnd
