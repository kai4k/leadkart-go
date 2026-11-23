-- LeadKart Go — companysettings module init (ADR 0070).
--
-- One versioned commerce policy per company, its full append-only history, and
-- the company's own GST rate list. Orders and Inventory hold event-fed replicas
-- of the policy (their own migrations); this schema is the single owner.
--
--   commerce_policy  one row per tenant: the stored version and every setting
--                    EXCEPT the GST rate list (that lives in gst_rates). A tenant
--                    with NO row reads the defaults at version 0 through
--                    commercepolicy.Initial; the row is created when the first
--                    version is stored (tenant registration seeds version 1).
--   policy_history   append-only: the WHOLE policy at each version, the changed
--                    keys, the acting membership, the impersonating operator if
--                    any, the reason, the time. The application role may INSERT
--                    and SELECT only (privileges.go TablePrivilegeExceptions).
--   gst_rates        the company's list: starts EMPTY, no default rate. A rate is
--                    confirmed by a named person and deactivated, never deleted.
--
-- All tenant-scoped, RLS + FORCE.

-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS companysettings;
COMMENT ON SCHEMA companysettings IS 'LeadKart companysettings module — versioned commerce policy, history and GST rate list (ADR 0070).';

CREATE TABLE companysettings.commerce_policy (
    tenant_id                uuid        PRIMARY KEY REFERENCES identity.tenants(id),
    version                  bigint      NOT NULL CHECK (version >= 0),
    -- every setting except tax.gst_rates (the gst_rates table is its source)
    policy                   jsonb       NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
    updated_at               timestamptz NOT NULL,
    updated_by_membership_id uuid        NULL
);

CREATE TABLE companysettings.policy_history (
    tenant_id                uuid        NOT NULL REFERENCES identity.tenants(id),
    version                  bigint      NOT NULL CHECK (version >= 1),
    -- the WHOLE policy at this version, GST rate list included
    policy                   jsonb       NOT NULL CHECK (jsonb_typeof(policy) = 'object'),
    changed_keys             text[]      NOT NULL DEFAULT '{}',
    actor_membership_id      uuid        NULL,
    impersonator_operator_id text        NULL CHECK (impersonator_operator_id IS NULL OR length(impersonator_operator_id) <= 100),
    reason                   text        NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    changed_at               timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, version)
);

CREATE TABLE companysettings.gst_rates (
    id           uuid        PRIMARY KEY,
    tenant_id    uuid        NOT NULL REFERENCES identity.tenants(id),
    rate_bps     integer     NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    label        text        NOT NULL CHECK (length(label) BETWEEN 1 AND 40),
    confirmed_by text        NOT NULL CHECK (length(confirmed_by) BETWEEN 1 AND 120),
    confirmed_on date        NOT NULL,
    active       boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL,
    -- the membership that added the rate (NULL only for a system-added rate)
    created_by_membership_id uuid NULL
);

-- The same rate under the same label once per company (case-insensitive).
CREATE UNIQUE INDEX uq_gst_rates_tenant_rate_label
    ON companysettings.gst_rates (tenant_id, rate_bps, lower(label));

-- Newest-first history page (keyset on version).
CREATE INDEX idx_policy_history_tenant_version
    ON companysettings.policy_history (tenant_id, version DESC);

ALTER TABLE companysettings.commerce_policy ENABLE ROW LEVEL SECURITY;
ALTER TABLE companysettings.commerce_policy FORCE ROW LEVEL SECURITY;
ALTER TABLE companysettings.policy_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE companysettings.policy_history FORCE ROW LEVEL SECURITY;
ALTER TABLE companysettings.gst_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE companysettings.gst_rates FORCE ROW LEVEL SECURITY;

CREATE POLICY commerce_policy_select ON companysettings.commerce_policy
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY commerce_policy_write ON companysettings.commerce_policy
    FOR ALL USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY policy_history_select ON companysettings.policy_history
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY policy_history_insert ON companysettings.policy_history
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY gst_rates_select ON companysettings.gst_rates
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY gst_rates_write ON companysettings.gst_rates
    FOR ALL USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE companysettings.commerce_policy IS
    'One versioned commerce policy per tenant (ADR 0070). No row = defaults at version 0. Tenant-scoped, FORCE RLS.';
COMMENT ON TABLE companysettings.policy_history IS
    'Append-only: the whole policy at every version, with actor, impersonator and reason. The application role holds SELECT and INSERT only.';
COMMENT ON TABLE companysettings.gst_rates IS
    'The company''s own GST rate list. Starts empty. Deactivated, never deleted.';

-- A NEW schema carries its own grants (see 20260606000001_application_role_grants.sql).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        RAISE NOTICE 'role leadkart_app does not exist: companysettings grants skipped';
        RETURN;
    END IF;
    GRANT USAGE ON SCHEMA companysettings TO leadkart_app;
    GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA companysettings TO leadkart_app;
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA companysettings TO leadkart_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA companysettings GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO leadkart_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA companysettings GRANT USAGE, SELECT ON SEQUENCES TO leadkart_app;
    -- the history is append-only for the application
    REVOKE UPDATE, DELETE ON companysettings.policy_history FROM leadkart_app;
END
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS companysettings.gst_rates CASCADE;
DROP TABLE IF EXISTS companysettings.policy_history CASCADE;
DROP TABLE IF EXISTS companysettings.commerce_policy CASCADE;
DROP SCHEMA IF EXISTS companysettings CASCADE;

-- +goose StatementEnd
