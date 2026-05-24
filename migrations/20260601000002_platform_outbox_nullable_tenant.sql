-- LeadKart Go — Phase 2 Slice 1 follow-up — platform.outbox tenant_id
-- becomes NULLABLE per ADR 0059 review-pass fix C3.
--
-- The slice-1 init migration declared `tenant_id uuid NOT NULL` + the
-- writer passed `uuid.Nil` (00000000-0000-0000-0000-000000000000) for
-- platform-scoped events. Two real problems followed:
--
--   1. Downstream consumers reading `tenant_id` saw a string that
--      LOOKS like a real UUID — they could legitimately try to fetch
--      a Tenant by that key + 404 / log noise.
--   2. The `platform_outbox_tenant_topic_occurred` composite index +
--      RLS predicate both touched a single sentinel value, creating
--      hot-spot churn under load on the platform-scoped event stream.
--
-- This migration:
--   - Drops the NOT NULL constraint on platform.outbox.tenant_id.
--   - Backfills existing uuid.Nil rows to NULL (lossless — they were
--     platform-scoped from the start).
--   - Adjusts RLS SELECT to allow NULL tenant_id rows to be visible
--     to the platform operator (the only legitimate consumer of
--     platform-scoped events at SELECT-time — the forwarder).
--
-- INSERT RLS keeps its existing shape — the WITH CHECK now also passes
-- when tenant_id IS NULL provided the caller is platform (i.e. the
-- adapter writes platform-scoped events under TxScopePlatform).
--
-- +goose Up
-- +goose StatementBegin

-- 1. Backfill any existing uuid.Nil sentinel rows to NULL before the
--    constraint change — otherwise the new RLS predicate would mark
--    them invisible to the platform operator's SELECT chain.
UPDATE platform.outbox
SET tenant_id = NULL
WHERE tenant_id = '00000000-0000-0000-0000-000000000000'::uuid;

-- 2. Drop NOT NULL — the column now carries NULL for platform-scoped
--    events + a real UUID for tenant-scoped events.
ALTER TABLE platform.outbox
    ALTER COLUMN tenant_id DROP NOT NULL;

-- 3. Replace the SELECT + INSERT policies to handle NULL tenant_id.
DROP POLICY IF EXISTS platform_outbox_select ON platform.outbox;
DROP POLICY IF EXISTS platform_outbox_insert ON platform.outbox;

CREATE POLICY platform_outbox_select ON platform.outbox
    FOR SELECT
    USING (
        tenant_id IS NULL AND app.is_platform()
        OR tenant_id = app.current_tenant()
        OR app.is_platform()
    );

CREATE POLICY platform_outbox_insert ON platform.outbox
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL AND app.is_platform()
        OR tenant_id = app.current_tenant()
        OR app.is_platform()
    );

COMMENT ON COLUMN platform.outbox.tenant_id IS
    'NULL = Platform-scoped event (no tenant). Non-NULL = TenantScoped event keyed to identity.tenants.id. Per ADR 0059 review-pass fix C3 (migration 20260601000002).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS platform_outbox_select ON platform.outbox;
DROP POLICY IF EXISTS platform_outbox_insert ON platform.outbox;

-- Re-introduce NOT NULL — restore the uuid.Nil sentinel for any rows
-- that were rewritten to NULL by the Up migration.
UPDATE platform.outbox
SET tenant_id = '00000000-0000-0000-0000-000000000000'::uuid
WHERE tenant_id IS NULL;

ALTER TABLE platform.outbox
    ALTER COLUMN tenant_id SET NOT NULL;

CREATE POLICY platform_outbox_select ON platform.outbox
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY platform_outbox_insert ON platform.outbox
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- +goose StatementEnd
