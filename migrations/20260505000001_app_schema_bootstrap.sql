-- LeadKart Go — App schema bootstrap
-- Purpose: DB-wide primitives consumed by every module's RLS policies.
-- Per ADR 0006 (multi-tenancy via Postgres RLS + SET LOCAL app.tenant_id)
-- and ADR 0005 (goose migrations).
--
-- Lives outside any module schema because every module references it.
-- MUST be applied first (ordered first by timestamp prefix).

-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS app;
COMMENT ON SCHEMA app IS 'LeadKart cross-cutting primitives (RLS GUCs + helper functions). Owned by leadkart_owner; granted to leadkart_app + leadkart_test for read-only access.';

-- Cross-cutting infra schema (ex-buildingblocks; matches internal/common/
-- per ADR 0067). Created here in the bootstrap so every later migration —
-- messaging_infra, dead_letter, audit columns — can target it.
CREATE SCHEMA IF NOT EXISTS common;
COMMENT ON SCHEMA common IS 'LeadKart cross-cutting plumbing (idempotency, audit log, dead-letter). Matches internal/common/ (TDL canon). Renamed from buildingblocks per ADR 0067.';

-- ----------------------------------------------------------------------------
-- LEAKPROOF wrapper around current_setting() so Postgres can push
-- `app.current_tenant()` predicates into index scans (without LEAKPROOF
-- the planner would force a sequential scan on every tenant-filtered
-- query — disastrous for large tables).
--
-- Returns NULL if the GUC is unset, which the RLS policies check
-- against to fail closed (no tenant set → no rows visible).
-- +goose StatementEnd
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.current_tenant() RETURNS uuid
    LANGUAGE sql
    STABLE
    LEAKPROOF
    AS $$
        SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid;
    $$;
-- +goose StatementEnd
COMMENT ON FUNCTION app.current_tenant() IS 'Returns the current tenant UUID from the app.tenant_id GUC (set per-tx by pgxpool AfterAcquire callback). LEAKPROOF — predicates push into indexes. Per ADR 0006.';

-- ----------------------------------------------------------------------------
-- Platform-operator bypass flag. Set to true on connection-acquire ONLY
-- for connections from the platform-operator pool (separate pgxpool with
-- different role + AfterAcquire). Tenant pool ALWAYS sets this to false
-- so a tenant request can never spoof platform privileges.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.is_platform() RETURNS boolean
    LANGUAGE sql
    STABLE
    LEAKPROOF
    AS $$
        -- NULLIF + COALESCE handles three cases identically:
        --   GUC never set       → current_setting returns NULL  → 'false'
        --   GUC set then reset  → current_setting returns ''     → 'false'
        --   GUC set explicitly  → current_setting returns 't'/'f'/'true'/'false'
        -- Without NULLIF, '' would hit ::boolean and fail SQLSTATE 22P02.
        SELECT COALESCE(NULLIF(current_setting('app.is_platform', true), ''), 'false')::boolean;
    $$;
-- +goose StatementEnd
COMMENT ON FUNCTION app.is_platform() IS 'Returns true when the connection is operating in platform-operator mode (separate connection pool + role). LEAKPROOF. Per ADR 0006.';

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS app.is_platform();
DROP FUNCTION IF EXISTS app.current_tenant();
DROP SCHEMA IF EXISTS common CASCADE;
DROP SCHEMA IF EXISTS app CASCADE;
-- +goose StatementEnd
