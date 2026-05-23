-- LeadKart Go — Wave 9.4 — Role hierarchy as join-table aggregate.
--
-- ADR 0058. SUPERSEDES ADR 0054. Hierarchy moves from a self-FK column
-- on identity.roles into its own aggregate: identity.role_hierarchy_edges.
-- Per Vernon IDDD ch.7 + Khorikov "Pragmatic Clean Architecture" §11 —
-- relationships with their own lifecycle, audit trail, or extension
-- potential (time-bound edges, approver chains) are aggregates, NOT
-- columns on one side.
--
-- Why the duct-tape SECURITY DEFINER trigger goes away:
--   The Wave 9.1d trigger had to bypass RLS to do its cross-tenant
--   compare. With the new shape, cross-tenant safety is DECLARATIVE
--   via composite FKs — the (tenant_id, X_role_id) → (tenant_id, id)
--   tuples natively forbid mixing tenants. The cycle trigger that
--   remains only walks ancestors WITHIN the current tenant + can run
--   SECURITY INVOKER under RLS.
--
-- Migration plan:
--   1. Add the (tenant_id, id) candidate-key UNIQUE to identity.roles
--      so the composite FK can target it.
--   2. Create identity.role_hierarchy_edges with composite FKs +
--      partial-unique single-parent index + RLS+FORCE.
--   3. Cycle-detection trigger on the edge table (SECURITY INVOKER —
--      same-tenant only, RLS-safe).
--   4. DATA-LIFT: copy every existing roles.parent_role_id link into
--      role_hierarchy_edges (audit metadata synthesised — reason marker
--      identifies migrated edges).
--   5. Drop the OLD trigger / function / index + the
--      roles.parent_role_id column.

-- +goose Up
-- +goose StatementBegin

-- 1. Candidate-key UNIQUE so composite FK can target (tenant_id, id).
ALTER TABLE identity.roles
    ADD CONSTRAINT uq_roles_tenant_id UNIQUE (tenant_id, id);

-- 2. The hierarchy aggregate's storage. Each row is ONE directed
--    parent→child edge. Tenant-scoped + RLS+FORCE (mirrors
--    role_assignments / permission_overrides / permission_requests).
CREATE TABLE identity.role_hierarchy_edges (
    id                            uuid        NOT NULL PRIMARY KEY,
    tenant_id                     uuid        NOT NULL,
    child_role_id                 uuid        NOT NULL,
    parent_role_id                uuid        NOT NULL,
    -- Audit columns. Reason is optional (NULL allowed) so internal-system
    -- edges (operator-set during onboarding) can elide it; when set the
    -- length floor matches the impersonation / permission_request reason
    -- canon (≥10 chars) per DPDP §12 + SOC2 CC4.1 audit guidance.
    established_at                timestamptz NOT NULL DEFAULT now(),
    established_by_membership_id  uuid        NULL,
    reason                        text        NULL CHECK (reason IS NULL OR (length(reason) BETWEEN 10 AND 1024)),
    -- Soft-delete. removed_at NULL = active edge; non-NULL = historical
    -- (kept for audit). Single-parent invariant enforced via the
    -- partial unique index below (only one ACTIVE edge per child).
    removed_at                    timestamptz NULL,
    removed_by_membership_id      uuid        NULL,
    removal_reason                text        NULL CHECK (removal_reason IS NULL OR (length(removal_reason) BETWEEN 10 AND 1024)),

    -- Cross-tenant safety — declarative; replaces the SECURITY DEFINER
    -- trigger from migrations 0002 + 0005. The composite
    -- (tenant_id, X_role_id) → (tenant_id, id) tuple FK natively
    -- enforces same-tenant for BOTH endpoints. ADR 0058.
    CONSTRAINT fk_edges_child_same_tenant
        FOREIGN KEY (tenant_id, child_role_id)
        REFERENCES identity.roles(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_edges_parent_same_tenant
        FOREIGN KEY (tenant_id, parent_role_id)
        REFERENCES identity.roles(tenant_id, id) ON DELETE CASCADE,
    -- Self-reference forbidden at the schema layer.
    CONSTRAINT chk_edge_no_self_loop CHECK (child_role_id <> parent_role_id)
);

COMMENT ON TABLE identity.role_hierarchy_edges IS
    'ADR 0058 — role-hierarchy aggregate (Wave 9.4). One row per directed parent→child edge. Soft-delete preserves history. Single-parent invariant via partial unique index uq_role_hierarchy_active_edge_per_child.';

-- 3. Single-parent invariant: at most ONE active edge per (tenant, child).
CREATE UNIQUE INDEX uq_role_hierarchy_active_edge_per_child
    ON identity.role_hierarchy_edges (tenant_id, child_role_id)
    WHERE removed_at IS NULL;

-- Read indexes — "show children of X" + "show parent of Y" queries.
CREATE INDEX idx_role_hierarchy_edges_parent
    ON identity.role_hierarchy_edges (tenant_id, parent_role_id)
    WHERE removed_at IS NULL;
CREATE INDEX idx_role_hierarchy_edges_child
    ON identity.role_hierarchy_edges (tenant_id, child_role_id)
    WHERE removed_at IS NULL;

-- 4. RLS+FORCE per LeadKart canon. Tenant-scoped reads + writes flow
--    through app.tenant_id GUC; platform-bypass via app.is_platform.
ALTER TABLE identity.role_hierarchy_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.role_hierarchy_edges FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_select ON identity.role_hierarchy_edges FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_insert ON identity.role_hierarchy_edges FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_update ON identity.role_hierarchy_edges FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_delete ON identity.role_hierarchy_edges FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

-- 5. DATA LIFT — preserve every existing roles.parent_role_id link as
--    an active edge before tearing the column down. The old column
--    carried no audit metadata so we synthesise: established_at = now,
--    established_by = NULL (system migration), reason = a fixed marker
--    so future audit queries can identify these migrated edges.
INSERT INTO identity.role_hierarchy_edges
    (id, tenant_id, child_role_id, parent_role_id, established_at,
     established_by_membership_id, reason)
SELECT
    gen_random_uuid(),
    r.tenant_id,
    r.id,
    r.parent_role_id,
    now(),
    NULL,
    'migrated from roles.parent_role_id at ADR 0058'
FROM identity.roles r
WHERE r.parent_role_id IS NOT NULL;

-- 6. Simplified cycle-detection trigger on the EDGE table. Cross-tenant
--    safety is now declarative via the composite FKs above; this
--    trigger only walks ancestors WITHIN the current tenant — RLS
--    visibility is guaranteed by the FK constraint that all edges in
--    a chain share the same tenant. SECURITY INVOKER suffices.
--
--    Catches the MULTI-HOP cycle case (A→B + B→A inserted sequentially;
--    the second insert closes the loop). Self-reference is already
--    blocked by chk_edge_no_self_loop above.
CREATE OR REPLACE FUNCTION identity.edge_check_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cur_id      uuid := NEW.parent_role_id;
    visited     uuid[] := ARRAY[NEW.child_role_id];
    step_parent uuid;
BEGIN
    -- Self-reference is handled by chk_edge_no_self_loop CHECK. Yielding
    -- here keeps the responsibilities clean (CHECK = single-row invariant,
    -- trigger = multi-row graph invariant) and lets the adapter
    -- discriminate self-loop from cycle by the constraint name.
    IF NEW.child_role_id = NEW.parent_role_id THEN
        RETURN NEW;
    END IF;
    WHILE cur_id IS NOT NULL LOOP
        IF cur_id = ANY(visited) THEN
            RAISE EXCEPTION 'role hierarchy: cycle detected at role %', cur_id
                USING ERRCODE = 'check_violation';
        END IF;
        visited := array_append(visited, cur_id);
        SELECT parent_role_id INTO step_parent
        FROM identity.role_hierarchy_edges
        WHERE tenant_id = NEW.tenant_id
          AND child_role_id = cur_id
          AND removed_at IS NULL;
        cur_id := step_parent;
    END LOOP;
    RETURN NEW;
END;
$$;

CREATE TRIGGER edge_check_cycle_trigger
    BEFORE INSERT ON identity.role_hierarchy_edges
    FOR EACH ROW
    EXECUTE FUNCTION identity.edge_check_cycle();

-- 7. Drop the now-redundant column + its trigger / function / index.
--    The SECURITY-DEFINER function from 20260523000005 also goes
--    because (a) the column it references no longer exists, and
--    (b) the new model uses identity.edge_check_cycle() on the edges
--    table instead.
DROP INDEX  IF EXISTS identity.idx_roles_parent_role_id;
DROP TRIGGER IF EXISTS role_check_hierarchy_trigger ON identity.roles;
DROP FUNCTION IF EXISTS identity.role_check_hierarchy();
ALTER TABLE identity.roles DROP COLUMN IF EXISTS parent_role_id;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the roles.parent_role_id adjacency-list column.
ALTER TABLE identity.roles
    ADD COLUMN parent_role_id uuid NULL
        REFERENCES identity.roles(id) ON DELETE SET NULL;

-- Re-populate parent_role_id from any ACTIVE edges before tearing
-- down the edges table — keeps the rollback round-trip lossless for
-- the dominant case.
UPDATE identity.roles r
SET parent_role_id = e.parent_role_id
FROM identity.role_hierarchy_edges e
WHERE e.child_role_id = r.id
  AND e.tenant_id     = r.tenant_id
  AND e.removed_at    IS NULL;

-- Restore the SECURITY DEFINER trigger from migration 0005 + its
-- index, so the down state matches what was on disk before 0007.
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

CREATE TRIGGER role_check_hierarchy_trigger
    BEFORE INSERT OR UPDATE OF parent_role_id ON identity.roles
    FOR EACH ROW
    EXECUTE FUNCTION identity.role_check_hierarchy();

CREATE INDEX idx_roles_parent_role_id
    ON identity.roles (parent_role_id)
    WHERE parent_role_id IS NOT NULL;

-- Tear down the edges table + its trigger.
DROP TRIGGER IF EXISTS edge_check_cycle_trigger ON identity.role_hierarchy_edges;
DROP FUNCTION IF EXISTS identity.edge_check_cycle();
DROP TABLE   IF EXISTS identity.role_hierarchy_edges;

ALTER TABLE identity.roles DROP CONSTRAINT IF EXISTS uq_roles_tenant_id;

-- +goose StatementEnd
