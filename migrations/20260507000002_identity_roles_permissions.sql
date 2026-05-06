-- LeadKart Go — Identity authorization extensions.
-- Per LeadKart .NET multi-tenancy.md "SuperUser god-mode" + coding-standards.md
-- "Permissions — closed-set construction" + architecture.md "Cross-cutting query
-- contracts → Hierarchy queries".
--
-- Adds:
--   identity.roles                              → tenant-scoped, RLS + FORCE
--   identity.role_assignments                   → tenant-scoped, RLS + FORCE,
--                                                 composite-FK to memberships
--   identity.membership_permission_overrides    → tenant-scoped, RLS + FORCE,
--                                                 composite-FK
--   identity.tenant_memberships  (alter)        → adds profile + hierarchy cols
--                                                 + (id, tenant_id) candidate-key
--                                                 + reports_to self-FK
--
-- Tenant-scoping verdicts (rationale per multi-tenancy.md "Identity model"):
--   identity.roles                              tenant-scoped (each tenant
--                                                 defines its own catalogue)
--   identity.role_assignments                   tenant-scoped (denormalised
--                                                 tenant_id, composite-FK to
--                                                 tenant_memberships(id, tenant_id))
--   identity.membership_permission_overrides    tenant-scoped (denormalised
--                                                 tenant_id, composite-FK)

-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- (1) (id, tenant_id) candidate-key on tenant_memberships
-- Prerequisite: downstream composite FKs reference (id, tenant_id) — Postgres
-- requires the referenced columns carry a UNIQUE or PRIMARY KEY constraint.
-- ============================================================================

ALTER TABLE identity.tenant_memberships
    ADD CONSTRAINT uq_memberships_id_tenant UNIQUE (id, tenant_id);

-- ============================================================================
-- (2) Profile + hierarchy columns on tenant_memberships
-- Mirrors Membership domain extensions from Phase 1 Task 14:
--   - designation, department, statusMessage (per-tenant profile)
--   - reports_to (manager Membership ID; self-FK below)
-- ============================================================================

ALTER TABLE identity.tenant_memberships
    ADD COLUMN designation    text NOT NULL DEFAULT '',
    ADD COLUMN department     text NOT NULL DEFAULT '',
    ADD COLUMN status_message text NOT NULL DEFAULT '',
    ADD COLUMN reports_to     uuid NULL;

-- ============================================================================
-- (3) Self-referential FK: reports_to manager MUST belong to same tenant
-- Mirrors the cross-tenant-mix-up prevention pattern documented in
-- database.md "Referential integrity for denormalised tenant_id".
-- ============================================================================

ALTER TABLE identity.tenant_memberships
    ADD CONSTRAINT fk_memberships_reports_to_same_tenant
    FOREIGN KEY (reports_to, tenant_id)
        REFERENCES identity.tenant_memberships(id, tenant_id);

CREATE INDEX idx_memberships_reports_to
    ON identity.tenant_memberships (reports_to)
    WHERE reports_to IS NOT NULL;

-- ============================================================================
-- (4) identity.roles — per-tenant Role catalogue
-- Mirrors Role aggregate (Phase 1 Tasks 5-10): id + tenant_id + name + flags
-- (system_default, super_admin) + hierarchy_level + permissions JSONB array
-- + audit (created_at, soft-delete fields).
--
-- permissions is `jsonb` (array of permission name strings). Repository
-- (Task 16) marshals from role.Permissions() → []string → json bytes.
-- ============================================================================

CREATE TABLE identity.roles (
    id                uuid        PRIMARY KEY,
    tenant_id         uuid        NOT NULL REFERENCES identity.tenants(id),
    name              text        NOT NULL CHECK (length(name) BETWEEN 2 AND 100),
    is_system_default boolean     NOT NULL DEFAULT false,
    is_super_admin    boolean     NOT NULL DEFAULT false,
    hierarchy_level   integer     NOT NULL CHECK (hierarchy_level BETWEEN 0 AND 99),
    permissions       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    created_at        timestamptz NOT NULL,
    is_deleted        boolean     NOT NULL DEFAULT false,
    deleted_at        timestamptz NULL,
    deleted_by        text        NULL
);

-- Names unique within a tenant only (recreatable after soft-delete).
CREATE UNIQUE INDEX uq_roles_tenant_name
    ON identity.roles (tenant_id, name) WHERE NOT is_deleted;
CREATE INDEX idx_roles_tenant ON identity.roles (tenant_id);
CREATE INDEX idx_roles_super_admin
    ON identity.roles (id) WHERE is_super_admin AND NOT is_deleted;

ALTER TABLE identity.roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.roles FORCE  ROW LEVEL SECURITY;

CREATE POLICY roles_select ON identity.roles
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY roles_insert ON identity.roles
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY roles_modify ON identity.roles
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY roles_delete ON identity.roles
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE identity.roles IS
    'Per-tenant Role catalogue. Tenant-scoped, FORCE RLS. SuperAdmin (is_super_admin=true) is seeded once in the platform tenant per multi-tenancy.md "SuperUser god-mode".';

-- ============================================================================
-- (5) identity.role_assignments — Membership ↔ Role junction
-- Tenant-scoped via denormalised tenant_id. Composite FK on
-- (membership_id, tenant_id) → tenant_memberships(id, tenant_id) guarantees
-- the membership's tenant matches the assignment's tenant — same anti-mix-up
-- pattern as documented in database.md.
-- ============================================================================

CREATE TABLE identity.role_assignments (
    membership_id  uuid        NOT NULL,
    role_id        uuid        NOT NULL REFERENCES identity.roles(id),
    tenant_id      uuid        NOT NULL,
    assigned_at    timestamptz NOT NULL,
    PRIMARY KEY (membership_id, role_id),
    FOREIGN KEY (membership_id, tenant_id)
        REFERENCES identity.tenant_memberships(id, tenant_id)
);

CREATE INDEX idx_role_assignments_role ON identity.role_assignments (role_id);
CREATE INDEX idx_role_assignments_tenant ON identity.role_assignments (tenant_id);

ALTER TABLE identity.role_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.role_assignments FORCE  ROW LEVEL SECURITY;

CREATE POLICY role_assignments_select ON identity.role_assignments
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY role_assignments_insert ON identity.role_assignments
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY role_assignments_modify ON identity.role_assignments
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY role_assignments_delete ON identity.role_assignments
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE identity.role_assignments IS
    'Membership ↔ Role junction with denormalised tenant_id. Composite FK ensures no cross-tenant role assignment.';

-- ============================================================================
-- (6) identity.membership_permission_overrides — per-Membership overlay
-- Effective permission set computed by Phase 1 Task 13 resolver:
--   union(role.Permissions for r in role_assignments)
--     ∪ overrides WHERE kind='granted'
--     \ overrides WHERE kind='revoked'
-- ============================================================================

CREATE TABLE identity.membership_permission_overrides (
    membership_id   uuid        NOT NULL,
    permission_name text        NOT NULL CHECK (length(permission_name) BETWEEN 3 AND 100),
    kind            text        NOT NULL CHECK (kind IN ('granted', 'revoked')),
    tenant_id       uuid        NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (membership_id, permission_name),
    FOREIGN KEY (membership_id, tenant_id)
        REFERENCES identity.tenant_memberships(id, tenant_id)
);

CREATE INDEX idx_perm_overrides_tenant ON identity.membership_permission_overrides (tenant_id);

ALTER TABLE identity.membership_permission_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.membership_permission_overrides FORCE  ROW LEVEL SECURITY;

CREATE POLICY perm_overrides_select ON identity.membership_permission_overrides
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY perm_overrides_insert ON identity.membership_permission_overrides
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY perm_overrides_modify ON identity.membership_permission_overrides
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY perm_overrides_delete ON identity.membership_permission_overrides
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE identity.membership_permission_overrides IS
    'Per-Membership permission overlay. Effective set = role union ∪ granted \ revoked.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS identity.membership_permission_overrides CASCADE;
DROP TABLE IF EXISTS identity.role_assignments CASCADE;
DROP TABLE IF EXISTS identity.roles CASCADE;
DROP INDEX IF EXISTS identity.idx_memberships_reports_to;
ALTER TABLE identity.tenant_memberships DROP CONSTRAINT IF EXISTS fk_memberships_reports_to_same_tenant;
ALTER TABLE identity.tenant_memberships DROP CONSTRAINT IF EXISTS uq_memberships_id_tenant;
ALTER TABLE identity.tenant_memberships
    DROP COLUMN IF EXISTS reports_to,
    DROP COLUMN IF EXISTS status_message,
    DROP COLUMN IF EXISTS department,
    DROP COLUMN IF EXISTS designation;
-- +goose StatementEnd
