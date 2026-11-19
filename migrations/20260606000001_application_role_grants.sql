-- LeadKart Go — privileges of the application runtime role (leadkart_app).
--
-- WHY THIS MIGRATION EXISTS. docker/postgres/init.sql creates the login role
-- leadkart_app and says "migrations issue the GRANTs", but no earlier migration
-- did. The production API (docker/compose.prod.yml) connects as leadkart_app, a
-- non-owner under FORCE ROW LEVEL SECURITY, so without these grants every query
-- fails with "permission denied for schema ...". Tests hid it by granting by
-- hand in their fixtures; dev hid it by running the API as the owner/superuser
-- (which also bypasses RLS). Grants now live HERE, and the fixtures run on
-- exactly what this migration grants.
--
-- WHAT leadkart_app GETS (and nothing more):
--   * USAGE on every application schema:
--       app, common, identity, platform, crm, inventory, shared, tasks,
--       dispatch, orders, notifications
--   * SELECT, INSERT, UPDATE, DELETE on all tables of every schema except app
--     (app holds only the RLS helper functions)
--   * USAGE, SELECT on all sequences of those schemas (serial / identity columns,
--     e.g. common.outbox."offset"; GRANT ON TABLES does not cover sequences)
--   * EXECUTE on the functions in schema app (app.current_tenant(),
--     app.is_platform(), called by every RLS policy)
--   * default privileges, so tables and sequences (and app functions) CREATED
--     LATER BY THE ROLE RUNNING THIS MIGRATION (the migration owner) in those
--     schemas are covered automatically
--   NEVER: TRUNCATE, REFERENCES, TRIGGER, DDL (no CREATE on any schema), object
--   ownership, SUPERUSER, or BYPASSRLS. goose's own version table needs nothing
--   from the app and gets nothing.
--
-- HOW A NEW MODULE STAYS COVERED. Default privileges are per schema and per
-- creating role: a new table in an EXISTING schema is covered with no further
-- work. A NEW schema is not (it has no USAGE grant and no default privileges),
-- so the new module's init migration must carry its own
--   GRANT USAGE ON SCHEMA <s> TO leadkart_app;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA <s> GRANT SELECT, INSERT, UPDATE, DELETE
--       ON TABLES TO leadkart_app;
--   ALTER DEFAULT PRIVILEGES IN SCHEMA <s> GRANT USAGE, SELECT ON SEQUENCES
--       TO leadkart_app;
-- (inside the same IF EXISTS-role guard as below, so it stays a no-op where the
-- role is absent). TestAppRole_HasPrivilegesOnEveryApplicationTable fails CI
-- when a schema or table is missed.
--
-- IDEMPOTENT: GRANT and ALTER DEFAULT PRIVILEGES are re-runnable. NO-OP where the
-- role does not exist (a database provisioned without docker/postgres/init.sql;
-- create the role first, then run migrations, or re-run this grant block).

-- +goose Up
-- +goose StatementBegin
DO $$
DECLARE
    s text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        RAISE NOTICE 'role leadkart_app does not exist: application grants skipped';
        RETURN;
    END IF;

    FOREACH s IN ARRAY ARRAY['app', 'common', 'identity', 'platform', 'crm',
                             'inventory', 'shared', 'tasks', 'dispatch', 'orders',
                             'notifications'] LOOP
        CONTINUE WHEN NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = s);
        EXECUTE format('GRANT USAGE ON SCHEMA %I TO leadkart_app', s);
        IF s = 'app' THEN
            EXECUTE 'GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app';
            EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT EXECUTE ON FUNCTIONS TO leadkart_app';
        ELSE
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO leadkart_app', s);
            EXECUTE format('GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I TO leadkart_app', s);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO leadkart_app', s);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT USAGE, SELECT ON SEQUENCES TO leadkart_app', s);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
DECLARE
    s text;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        RETURN;
    END IF;

    FOREACH s IN ARRAY ARRAY['app', 'common', 'identity', 'platform', 'crm',
                             'inventory', 'shared', 'tasks', 'dispatch', 'orders',
                             'notifications'] LOOP
        CONTINUE WHEN NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = s);
        IF s = 'app' THEN
            EXECUTE 'ALTER DEFAULT PRIVILEGES IN SCHEMA app REVOKE EXECUTE ON FUNCTIONS FROM leadkart_app';
            EXECUTE 'REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA app FROM leadkart_app';
        ELSE
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I REVOKE USAGE, SELECT ON SEQUENCES FROM leadkart_app', s);
            EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM leadkart_app', s);
            EXECUTE format('REVOKE USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %I FROM leadkart_app', s);
            EXECUTE format('REVOKE SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I FROM leadkart_app', s);
        END IF;
        EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM leadkart_app', s);
    END LOOP;
END
$$;
-- +goose StatementEnd
