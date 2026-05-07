-- LeadKart Go — Admin Impersonation Audit table.
--
-- Per `multi-tenancy.md` "Impersonation": every per-request
-- impersonation activity (per operator session) writes an audit row.
-- 7-year retention per SOC2 CC4.1 + DPDP §12 — purge job lives in a
-- post-launch operational concern.
--
-- Cross-cutting non-tenanted store; lives in the buildingblocks
-- schema parallel to where the .NET side keeps Marten audit
-- documents declared SingleTenanted on the main store.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE buildingblocks.admin_impersonation_audit (
    id                uuid        PRIMARY KEY,
    session_id        uuid        NULL,
    operator_user_id  uuid        NOT NULL,
    target_tenant_id  uuid        NOT NULL,
    correlation_id    text        NOT NULL DEFAULT '',
    http_route        text        NOT NULL DEFAULT '',
    http_method       text        NOT NULL DEFAULT '',
    reason            text        NOT NULL,
    started_at_utc    timestamptz NOT NULL,
    is_god_mode       boolean     NOT NULL DEFAULT false
);

CREATE INDEX idx_admin_impersonation_audit_operator
    ON buildingblocks.admin_impersonation_audit (operator_user_id, started_at_utc DESC);

CREATE INDEX idx_admin_impersonation_audit_target_tenant
    ON buildingblocks.admin_impersonation_audit (target_tenant_id, started_at_utc DESC);

CREATE INDEX idx_admin_impersonation_audit_session
    ON buildingblocks.admin_impersonation_audit (session_id)
    WHERE session_id IS NOT NULL;

COMMENT ON TABLE buildingblocks.admin_impersonation_audit IS
    'Per-request operator impersonation activity. Indexed on operator + target tenant for forensic queries. 7-year retention per SOC2 CC4.1 / DPDP §12.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS buildingblocks.admin_impersonation_audit;

-- +goose StatementEnd
