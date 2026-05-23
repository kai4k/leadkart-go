-- LeadKart Go — Hotfix on Wave 9.1d's role-hierarchy trigger (ADR 0054).
--
-- The original trigger function `identity.role_check_hierarchy()` from
-- migration 20260523000002 runs as SECURITY INVOKER (the PL/pgSQL
-- default). Under RLS+FORCE on identity.roles, the function's
-- internal lookup `SELECT tenant_id FROM identity.roles WHERE id =
-- NEW.parent_role_id` is silently filtered when the caller's session
-- is scoped to a DIFFERENT tenant than the parent's. The trigger sees
-- the parent as "doesn't exist" + falls through to the FK constraint
-- (which DOES exist but checks bypass RLS) — net result: a tenant-A
-- role can be reparented to a tenant-B role without rejection.
--
-- Integration test surface that exposed this:
--   TestRoleRepository_Hierarchy_TriggerRejectsCrossTenant — got <nil>
--   instead of role.ErrHierarchyCrossTenant on the UPDATE attempt.
--
-- Fix: recreate the function as SECURITY DEFINER. Migrations run as
-- the database owner (POSTGRES_USER per docker compose; in CI, the
-- ephemeral pg's owner role) which has implicit BYPASSRLS — so the
-- function's internal SELECTs see ALL rows regardless of session
-- tenant scope. The cross-tenant + cycle checks now both work.
--
-- Pinned search_path per PostgreSQL canonical SECURITY DEFINER
-- guidance (Database Administrator's Guide §38.7) — prevents
-- search-path-substitution privilege escalation.
--
-- Per Vladimir Khorikov §11: DB-level invariant enforcement is the
-- LAST line of defense. Wave 9.1d shipped the trigger but the RLS
-- interaction wasn't tested locally (//go:build integration);
-- cloud CI surfaced the gap. ADR 0054's three-layer cycle prevention
-- now actually works at the DB layer.

-- +goose Up
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION identity.role_check_hierarchy() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, identity
AS $$
DECLARE
    cur_id        uuid := NEW.parent_role_id;
    visited       uuid[] := ARRAY[NEW.id];
    parent_tenant uuid;
    step_parent   uuid;
BEGIN
    IF cur_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- Cross-tenant parent forbidden. SECURITY DEFINER lets this SELECT
    -- see rows across every tenant — without it, RLS would hide the
    -- candidate parent when the session is scoped to a different
    -- tenant + the trigger would silently miss the violation.
    SELECT tenant_id INTO parent_tenant FROM identity.roles WHERE id = cur_id;
    IF parent_tenant IS NULL THEN
        -- Parent truly doesn't exist (FK will raise its own error).
        RETURN NEW;
    END IF;
    IF parent_tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'role hierarchy: parent role belongs to a different tenant'
            USING ERRCODE = 'check_violation';
    END IF;

    -- Cycle walk — same RLS concern; SECURITY DEFINER bypasses.
    WHILE cur_id IS NOT NULL LOOP
        IF cur_id = ANY(visited) THEN
            RAISE EXCEPTION 'role hierarchy: cycle detected at role %', cur_id
                USING ERRCODE = 'check_violation';
        END IF;
        visited := array_append(visited, cur_id);
        SELECT parent_role_id INTO step_parent FROM identity.roles WHERE id = cur_id;
        cur_id := step_parent;
    END LOOP;

    RETURN NEW;
END;
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Revert to SECURITY INVOKER (the pre-hotfix behaviour — cross-tenant
-- check silently broken). Use only if rolling back the whole role-
-- hierarchy migration; otherwise leave SECURITY DEFINER in place.
CREATE OR REPLACE FUNCTION identity.role_check_hierarchy() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cur_id        uuid := NEW.parent_role_id;
    visited       uuid[] := ARRAY[NEW.id];
    parent_tenant uuid;
    step_parent   uuid;
BEGIN
    IF cur_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT tenant_id INTO parent_tenant FROM identity.roles WHERE id = cur_id;
    IF parent_tenant IS NULL THEN
        RETURN NEW;
    END IF;
    IF parent_tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'role hierarchy: parent role belongs to a different tenant'
            USING ERRCODE = 'check_violation';
    END IF;
    WHILE cur_id IS NOT NULL LOOP
        IF cur_id = ANY(visited) THEN
            RAISE EXCEPTION 'role hierarchy: cycle detected at role %', cur_id
                USING ERRCODE = 'check_violation';
        END IF;
        visited := array_append(visited, cur_id);
        SELECT parent_role_id INTO step_parent FROM identity.roles WHERE id = cur_id;
        cur_id := step_parent;
    END LOOP;
    RETURN NEW;
END;
$$;

-- +goose StatementEnd
