-- LeadKart Go — Wave 9.1d — Role hierarchy (parent_role_id + cycle gate).
--
-- ADR 0054. Adds SINGLE-PARENT inheritance to identity.roles. A role's
-- EFFECTIVE permission set becomes its own grants ∪ every ancestor's
-- grants walked transitively. NULL parent_role_id = root (no inheritance).
--
-- Three-layer cycle prevention per ADR 0054:
--
--   1. DB-level trigger (this migration) — strict gate; walks the chain
--      upward + aborts on cycle OR cross-tenant parent.
--   2. Domain-level guard (role.Role.ChangeParent + ErrRoleHierarchyCycle)
--      — clean Go-side error for app handlers.
--   3. App-level pre-check (RoleRepository.GetAncestors) — best
--      ergonomic error message in the handler before the trigger fires.
--
-- Single-parent tree (NOT multi-parent DAG) is the conscious choice:
-- Microsoft Entra ID hierarchical roles + AWS IAM permission boundaries
-- (when hierarchical) + Salesforce Profile + Role Hierarchy all use
-- single-parent. Multi-parent over-engineers v0.2's flat catalog.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.roles
    ADD COLUMN parent_role_id uuid NULL REFERENCES identity.roles(id) ON DELETE SET NULL;

COMMENT ON COLUMN identity.roles.parent_role_id IS
    'ADR 0054 — single-parent hierarchy. NULL = root (no inheritance). Effective permission set walks the chain upward + unions grants. Cycle + cross-tenant prevented by identity.role_check_hierarchy() trigger.';

-- Cycle-prevention trigger function. Walks parent_role_id upward;
-- aborts on cycle OR cross-tenant parent. Fires per row on INSERT and
-- UPDATE OF parent_role_id (other UPDATEs skip the cost).
--
-- Per Vladimir Khorikov "Pragmatic Clean Architecture" §11: DB-level
-- invariant enforcement is the LAST line; the domain guard is the
-- FIRST. Both protect; only this one is FORCE-able.
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

    -- Cross-tenant parent forbidden — defense-in-depth alongside RLS
    -- (RLS would already hide the parent row when written through a
    -- tenant-scoped tx, but a platform-scoped writer could otherwise
    -- mix tenants; this guard makes it impossible).
    SELECT tenant_id INTO parent_tenant FROM identity.roles WHERE id = cur_id;
    IF parent_tenant IS NULL THEN
        -- Parent doesn't exist — let the FK constraint surface the error.
        RETURN NEW;
    END IF;
    IF parent_tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'role hierarchy: parent role belongs to a different tenant'
            USING ERRCODE = 'check_violation';
    END IF;

    -- Cycle walk — append each visited ancestor; abort if we ever see
    -- NEW.id again. Bounded by the tenant's role count; the catalogue
    -- is small per tenant so the loop is cheap.
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

CREATE TRIGGER role_check_hierarchy_trigger
    BEFORE INSERT OR UPDATE OF parent_role_id ON identity.roles
    FOR EACH ROW
    EXECUTE FUNCTION identity.role_check_hierarchy();

-- Performance: supports "list children of X" reverse queries. The
-- cycle trigger walks parent-of-child via the PK lookup — no index
-- needed for that direction.
CREATE INDEX idx_roles_parent_role_id
    ON identity.roles (parent_role_id)
    WHERE parent_role_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS identity.idx_roles_parent_role_id;
DROP TRIGGER IF EXISTS role_check_hierarchy_trigger ON identity.roles;
DROP FUNCTION IF EXISTS identity.role_check_hierarchy();
ALTER TABLE identity.roles DROP COLUMN IF EXISTS parent_role_id;
-- +goose StatementEnd
