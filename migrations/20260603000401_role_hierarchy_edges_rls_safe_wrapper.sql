-- LeadKart Go — Replace raw current_setting::boolean casts in
-- role_hierarchy_edges RLS policies with the safe app.is_platform()
-- wrapper.
--
-- THE BUG (migration 20260523000007):
--
--   USING (tenant_id = current_setting('app.tenant_id', true)::uuid
--          OR current_setting('app.is_platform', true)::boolean IS TRUE);
--
-- When `app.is_platform` is NOT set on the connection (the canonical
-- state for tenant-scoped flows that go through WithinTxPgxTenant /
-- TxScopeTenant), `current_setting('app.is_platform', true)` returns
-- empty string `''` and the `::boolean` cast fails with SQLSTATE 22P02
-- "invalid input syntax for type boolean". Production handlers
-- normally short-circuit the OR on the left (tenant_id match) before
-- the right side is evaluated, but rows that DON'T match tenant_id
-- (cross-tenant attempts, RLS-hidden rows in mixed-tenant queries)
-- trigger the cast → query crashes instead of returning empty.
--
-- THE FIX:
--
-- Use the safe wrapper `app.is_platform()` (defined in
-- 20260505000001_app_schema_bootstrap.sql), which COALESCEs the
-- missing/empty case to 'false' before casting:
--
--   SELECT COALESCE(NULLIF(current_setting('app.is_platform', true), ''),
--                   'false')::boolean;
--
-- Every other tenant-scoped table uses this wrapper. role_hierarchy_edges
-- (ADR 0058, Wave 9.4) was the only table whose RLS policies inlined
-- the raw cast. This migration brings it into line with the canonical
-- pattern + restores the property "tenant-scoped queries succeed
-- regardless of whether app.is_platform is set".
--
-- ALTER POLICY (not DROP+CREATE): swap the USING / WITH CHECK
-- expressions atomically inside the goose tx. DROP+CREATE would leave
-- a brief window where the policy is missing — under FORCE RLS that
-- means rows are temporarily invisible, which is benign inside a tx
-- but unidiomatic. ALTER POLICY is the canonical "swap predicate"
-- shape per PostgreSQL docs §sql-alterpolicy.
--
-- Surfaced by TestEdgeRepository_GetAncestorsByChild_RecursiveCTEWalksUpward
-- after pgtest.RunMain ordering fix (commit 7cda757) let integration
-- tests actually run against the schema for the first time.

-- +goose Up
-- +goose StatementBegin

ALTER POLICY tenant_isolation_select ON identity.role_hierarchy_edges
    USING (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_insert ON identity.role_hierarchy_edges
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_update ON identity.role_hierarchy_edges
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

ALTER POLICY tenant_isolation_delete ON identity.role_hierarchy_edges
    USING (tenant_id = app.current_tenant() OR app.is_platform());

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER POLICY tenant_isolation_select ON identity.role_hierarchy_edges
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_insert ON identity.role_hierarchy_edges
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_update ON identity.role_hierarchy_edges
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);

ALTER POLICY tenant_isolation_delete ON identity.role_hierarchy_edges
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

-- +goose StatementEnd
