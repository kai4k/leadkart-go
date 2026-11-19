-- LeadKart Go — permission_requests RLS policies use the safe tenant helpers.
--
-- WHY. migration 20260523000003 wrote the four policies of
-- identity.permission_requests with raw casts:
--     tenant_id = current_setting('app.tenant_id', true)::uuid
--     OR current_setting('app.is_platform', true)::boolean IS TRUE
-- A pooled connection that earlier ran a platform-scoped transaction reads the
-- (transaction-local) setting as '' afterwards, and '' cast to boolean raises
-- 22P02. A tenant-scoped read of another tenant's request id therefore answered
-- 500 instead of 404, and the error was an existence signal for foreign ids.
-- migration 20260603000401 fixed the same class for identity.role_hierarchy_edges
-- by moving to app.current_tenant() / app.is_platform() (NULLIF + COALESCE,
-- LEAKPROOF). This does the same for permission_requests, the only policy left
-- with a raw cast (TestRLS_NoPolicyUsesRawCurrentSetting sweeps pg_policies so the
-- class cannot return).

-- +goose Up
-- +goose StatementBegin

ALTER POLICY tenant_isolation_select ON identity.permission_requests
    USING (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_insert ON identity.permission_requests
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_update ON identity.permission_requests
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_delete ON identity.permission_requests
    USING (tenant_id = app.current_tenant() OR app.is_platform());

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER POLICY tenant_isolation_select ON identity.permission_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_insert ON identity.permission_requests
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_update ON identity.permission_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_delete ON identity.permission_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

-- +goose StatementEnd
