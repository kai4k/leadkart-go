-- LeadKart Go — replace `tenants.admin_email` denormalisation with a
-- proper audit chain on the existing membership graph.
--
-- Why: `identity.tenants.admin_email` was a snapshot of the registering
-- admin's email taken at RegisterTenantCommand time. It NEVER auto-
-- synced when the admin's Person email changed via the
-- request-email-change → confirm-email-change flow. Two-source-of-truth
-- bug waiting to happen + a column that lies after the first email
-- rotation.
--
-- Fix (industry canon — Stripe / Plaid / Salesforce shape):
--
--   1. DROP `tenants.admin_email`. The "current admin email" is a
--      derived value: tenant → CompanyOwner-role membership → person.email.
--      Platform-admin queries do the JOIN at read time. One source of
--      truth, no sync hazard.
--
--   2. ADD `tenant_memberships.created_by_membership_id` UUID NULL.
--      Tracks the audit chain "who invited / created this user."
--      NULL means system-bootstrapped:
--        - SuperAdmin (cmd/bootstrap)               → NULL
--        - First admin during tenant onboarding     → NULL (self-bootstrapped)
--        - Anyone invited by an existing user       → that user's membership_id
--      Composite FK ties (created_by, tenant_id) → tenant_memberships(id, tenant_id)
--      so a Tenant-A admin can't accidentally appear as creator of a
--      Tenant-B membership (cross-tenant audit-chain spoofing).
--
--   3. ADD `persons.created_by_person_id` UUID NULL.
--      Same audit dimension at the global Person level. NULL means
--      system-bootstrapped (Person not created via a logged-in user
--      action — RegisterTenant's first admin, the SuperAdmin).
--
-- Hierarchy (`reports_to` on tenant_memberships) already exists from
-- migration 20260507000005 and is a separate concern (org chart, not
-- audit). This migration leaves it untouched.

-- +goose Up
-- +goose StatementBegin

-- (1) Drop the denormalised admin_email — the JOIN through
-- tenant_memberships → role_assignments → roles (WHERE name='CompanyOwner')
-- → persons gives us the current admin email at read time.
ALTER TABLE identity.tenants
    DROP COLUMN admin_email;

-- (2) Audit chain on memberships. NULL allowed for bootstrap rows.
ALTER TABLE identity.tenant_memberships
    ADD COLUMN created_by_membership_id uuid NULL;

-- Composite FK prevents cross-tenant audit-chain spoofing — the
-- referenced creator-membership MUST live in the same tenant as the
-- created membership. Same anti-mix-up shape role_assignments uses.
ALTER TABLE identity.tenant_memberships
    ADD CONSTRAINT fk_memberships_created_by
        FOREIGN KEY (created_by_membership_id, tenant_id)
        REFERENCES identity.tenant_memberships (id, tenant_id);

CREATE INDEX idx_memberships_created_by
    ON identity.tenant_memberships (created_by_membership_id)
    WHERE created_by_membership_id IS NOT NULL;

COMMENT ON COLUMN identity.tenant_memberships.created_by_membership_id IS
    'Audit chain — Membership of the user who created this row. NULL = system-bootstrapped (SuperAdmin, first-admin during tenant onboarding). Composite FK to (id, tenant_id) prevents cross-tenant spoofing.';

-- (3) Audit chain on persons. Cross-tenant context (Person is global,
-- non-RLS), so a single FK to persons.id is the right shape.
ALTER TABLE identity.persons
    ADD COLUMN created_by_person_id uuid NULL
        REFERENCES identity.persons(id);

CREATE INDEX idx_persons_created_by
    ON identity.persons (created_by_person_id)
    WHERE created_by_person_id IS NOT NULL;

COMMENT ON COLUMN identity.persons.created_by_person_id IS
    'Audit chain — Person of the user who created this row globally. NULL = system-bootstrapped (SuperAdmin, first-admin via RegisterTenant). Allows cross-tenant lookup of "who originally onboarded this user."';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reversing the audit-chain ADDs is straightforward. Restoring
-- admin_email requires backfilling from the JOIN — only meaningful
-- if there are real rows. For dev-test wipes the COALESCE makes the
-- restore safe even when no admin Membership exists yet.

ALTER TABLE identity.persons
    DROP COLUMN created_by_person_id;

ALTER TABLE identity.tenant_memberships
    DROP CONSTRAINT fk_memberships_created_by;

ALTER TABLE identity.tenant_memberships
    DROP COLUMN created_by_membership_id;

ALTER TABLE identity.tenants
    ADD COLUMN admin_email text NOT NULL DEFAULT '';

UPDATE identity.tenants t
SET    admin_email = COALESCE((
           SELECT p.email
           FROM   identity.tenant_memberships m
           JOIN   identity.role_assignments  ra ON ra.membership_id = m.id
           JOIN   identity.roles             r  ON r.id = ra.role_id
           JOIN   identity.persons           p  ON p.id = m.person_id
           WHERE  m.tenant_id = t.id
             AND  r.name      = 'CompanyOwner'
             AND  m.status    = 'active'
           LIMIT  1
       ), '');

ALTER TABLE identity.tenants
    ALTER COLUMN admin_email DROP DEFAULT;

ALTER TABLE identity.tenants
    ADD CONSTRAINT tenants_admin_email_check
        CHECK (length(admin_email) <= 254);

-- +goose StatementEnd
