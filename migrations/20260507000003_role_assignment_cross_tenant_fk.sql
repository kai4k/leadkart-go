-- LeadKart Go — Cross-tenant role-assignment FK enforcement.
-- Per LeadKart .NET database.md "Referential integrity for denormalised
-- tenant_id" + multi-tenancy.md "Composite FK pattern".
--
-- Closes a schema gap exposed by
-- TestMembershipRepository_Add_RejectsCrossTenantRoleAssignment:
-- migration 20260507000002 wired the composite FK on
-- (membership_id, tenant_id) → tenant_memberships(id, tenant_id) but kept
-- a simple role_id FK (→ roles(id)). That check passes whenever the role
-- exists in ANY tenant, so a Tenant-B role could be assigned to a
-- Tenant-A Membership — no cross-tenant rejection at the schema level.
--
-- Fix: composite FK on (role_id, tenant_id) → roles(id, tenant_id).
-- Requires roles to expose (id, tenant_id) as UNIQUE first (Postgres
-- requires the referenced columns carry a UNIQUE or PRIMARY KEY).
--
-- This is the same anti-mix-up pattern that membership_permission_overrides
-- already enforces via its composite FK to tenant_memberships.

-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- (1) (id, tenant_id) candidate-key on identity.roles
-- Prerequisite for the composite FK below. roles.id is already PRIMARY KEY,
-- so this UNIQUE is the auxiliary candidate-key — same shape as the
-- uq_memberships_id_tenant constraint added in 20260507000002.
-- ============================================================================

ALTER TABLE identity.roles
    ADD CONSTRAINT uq_roles_id_tenant UNIQUE (id, tenant_id);

-- ============================================================================
-- (2) Drop the simple role_id FK on role_assignments and replace with
--     composite FK on (role_id, tenant_id) → roles(id, tenant_id).
--
-- The existing constraint name is the implicit Postgres-generated name
-- (role_assignments_role_id_fkey). DROP IF EXISTS guards against
-- environments where a manual rename has happened.
-- ============================================================================

ALTER TABLE identity.role_assignments
    DROP CONSTRAINT IF EXISTS role_assignments_role_id_fkey;

ALTER TABLE identity.role_assignments
    ADD CONSTRAINT fk_role_assignments_role_same_tenant
    FOREIGN KEY (role_id, tenant_id)
        REFERENCES identity.roles(id, tenant_id);

COMMENT ON CONSTRAINT fk_role_assignments_role_same_tenant
    ON identity.role_assignments IS
    'Cross-tenant role assignment is impossible at the schema level — '
    'the composite FK forces the assignment''s tenant_id to match the '
    'role''s tenant_id. Mirrors the (membership_id, tenant_id) composite FK.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.role_assignments
    DROP CONSTRAINT IF EXISTS fk_role_assignments_role_same_tenant;

-- Restore the simple FK so a rollback leaves a working schema.
ALTER TABLE identity.role_assignments
    ADD CONSTRAINT role_assignments_role_id_fkey
    FOREIGN KEY (role_id) REFERENCES identity.roles(id);

ALTER TABLE identity.roles
    DROP CONSTRAINT IF EXISTS uq_roles_id_tenant;

-- +goose StatementEnd
