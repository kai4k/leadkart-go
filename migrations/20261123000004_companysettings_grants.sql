-- LeadKart Go — privileges of the application role on the companysettings schema
-- (ADR 0070).
--
-- A new schema is not covered by 20260606000001_application_role_grants (that
-- migration names the schemas that existed when it was written, and a migration
-- may not reference a schema created by a later one), so the module carries its
-- own grants, exactly the shape that migration documents:
--   * USAGE on the schema, SELECT/INSERT/UPDATE/DELETE on its tables, USAGE and
--     SELECT on its sequences, default privileges for tables added later;
--   * companysettings.policy_history is the append-only version ledger: SELECT and
--     INSERT only (pg.TablePrivilegeExceptions pins it).
--
-- It lives in its own migration, not in the schema's init, so the documented
-- recovery for a role created AFTER migrating is one more idempotent block to
-- run by hand (docs/deploy/production-checklist.md, "The application role").
--
-- IDEMPOTENT: GRANT, REVOKE and ALTER DEFAULT PRIVILEGES are re-runnable. A NO-OP
-- where the role does not exist (a database provisioned without
-- docker/postgres/init.sql); the start-up check then fails fast.

-- +goose Up
-- +goose StatementBegin
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
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        RETURN;
    END IF;
    ALTER DEFAULT PRIVILEGES IN SCHEMA companysettings REVOKE USAGE, SELECT ON SEQUENCES FROM leadkart_app;
    ALTER DEFAULT PRIVILEGES IN SCHEMA companysettings REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM leadkart_app;
    REVOKE USAGE, SELECT ON ALL SEQUENCES IN SCHEMA companysettings FROM leadkart_app;
    REVOKE SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA companysettings FROM leadkart_app;
    REVOKE USAGE ON SCHEMA companysettings FROM leadkart_app;
END
$$;
-- +goose StatementEnd
