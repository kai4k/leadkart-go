-- LeadKart Go — inventory.policy_replica (ADR 0070).
--
-- Inventory local, event-fed copy of a company's commerce policy. The single owner
-- is the companysettings module; this table is what Inventory READS, inside its own
-- business transaction, so a business rule is never fetched across a module
-- boundary while a row lock is held (no second pooled connection).
--
--   * Written only by the companysettings.policy_changed.v1 subscriber, which
--     stores a version only when it is NEWER than the stored one, so a duplicate
--     or reordered event changes nothing.
--   * A tenant with NO row reads commercepolicy.Initial (the defaults at
--     version 0); no row is ever stored at version 0.
--   * The daily republish job in companysettings re-announces every stored
--     version, so a dead-lettered event cannot leave this table stale for long.
--
-- Tenant-scoped, RLS + FORCE. No author column: the row is a projection of an
-- event, not a human action (the actor is in companysettings.policy_history).

-- +goose Up
-- +goose StatementBegin

CREATE TABLE inventory.policy_replica (
    tenant_id  uuid        PRIMARY KEY,
    version    bigint      NOT NULL CHECK (version >= 1),
    policy     jsonb       NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE inventory.policy_replica ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory.policy_replica FORCE ROW LEVEL SECURITY;

CREATE POLICY inventory_policy_replica_select ON inventory.policy_replica
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY inventory_policy_replica_insert ON inventory.policy_replica
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY inventory_policy_replica_update ON inventory.policy_replica
    FOR UPDATE USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE inventory.policy_replica IS
    'Event-fed replica of the company commerce policy (ADR 0070). Upserted only when the event version is newer. No row = defaults at version 0. Tenant-scoped, FORCE RLS.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS inventory.policy_replica CASCADE;

-- +goose StatementEnd
