-- =========================================================================
-- LeadKart Go — PostgreSQL initialization (docker-entrypoint-initdb.d).
-- =========================================================================
-- Runs ONCE when the data volume is empty, AS the postgres superuser the
-- entrypoint creates. Both compose.yml (dev) and compose.prod.yml mount
-- this file into /docker-entrypoint-initdb.d/.
--
-- Scope (deliberately minimal — `database.md` "Single schema writer"
-- says MigrationRunner owns ALL schema):
--   1. Enable pg_stat_statements extension (compose `command:` preloads
--      the .so but the EXTENSION must be installed once).
--   2. Create the leadkart_app + leadkart_test runtime roles. Migrations
--      run as the owner (POSTGRES_USER) and GRANT to these names — they
--      MUST exist when the migrate container runs.
--
-- Out of scope (NEVER add here — owned by migrations):
--   - CREATE SCHEMA (migrations 20260505000001 + 20260505000002 + ...)
--   - app.current_tenant() / app.is_platform() (migration 20260505000001)
--   - GRANT statements on specific schemas (migrations issue them after
--     creating the schemas)
--
-- Password hygiene:
--   The passwords below are PLACEHOLDERS for first-boot. Production MUST
--   rotate via:
--     ALTER ROLE leadkart_app WITH PASSWORD '<from secrets store>';
--     ALTER ROLE leadkart_test WITH PASSWORD '<dev-only or test-runner>';
--   Document the rotation in docs/runbooks/postgres-bootstrap.md once it
--   ships. Until then, treat the placeholders as throwaway dev creds.
-- =========================================================================

-- Extension prerequisites
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Runtime roles per multi-tenancy.md "Three Postgres roles"
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'leadkart_app') THEN
        CREATE ROLE leadkart_app
            WITH LOGIN
                 NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB
                 PASSWORD 'leadkart_app_dev_placeholder_rotate_in_prod';
    END IF;

    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'leadkart_test') THEN
        CREATE ROLE leadkart_test
            WITH LOGIN
                 NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB
                 PASSWORD 'leadkart_test_dev_placeholder_rotate_in_prod';
    END IF;
END
$$;
