-- ADR 0069 (H1/H5) — rank tiers + manager-tier daily-work grants for EXISTING
-- tenants' seeded roles. New tenants receive the same shape from
-- seed.DefaultRoleCatalog at registration.
--
-- Pristine-seed detection (never overwrite a tenant's customisation):
--   (1) the name is a DefaultRoleCatalog name, NOT is_deleted, NOT is_system_default;
--   (2) created_at equals the tenant's system-default 'Company Owner' role's
--       created_at — ApplyDefaultRoles stamps the whole batch with one `now`,
--       so a tenant-created homonym (or a renamed-then-recreated role) is newer;
--   (3a) hierarchy_level changes only while it still equals the old seed level;
--   (3b) permissions change only while the SET still equals the old seed set
--        (jsonb containment both ways). Each test is independent.
-- tenant.admin is deliberately NOT added to existing Administrator / Office
-- Administrator roles (ADR 0069 Q2) — owners opt in. Data-only migration: no
-- outbox/audit rows are written (recorded in ADR 0069).
--
-- Re-levelling custom level-0 roles. Level 0 is the Owner tier and belongs to
-- the system CompanyOwner role alone. The authority policy already ranks any
-- other level-0 role as 1 (authority.levelOf), so here the data is made to
-- agree: every role with hierarchy_level = 0 that is not the system
-- CompanyOwner role is moved to level 1. This includes soft-deleted roles and
-- the bootstrap SuperAdmin role (the policy treats it as 1 too; cmd/bootstrap
-- still seeds it at 0 on a fresh environment, harmless for the same reason).
-- THE DOWN MIGRATION CANNOT RESTORE THIS: the old level was 0 for every such
-- row, but after Up a role that was at 1 originally is indistinguishable from
-- one moved from 0, so Down leaves level 1 in place.

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

WITH spec(name, old_level, new_level, old_perms, add_perms) AS (
    VALUES
    ('Administrator', 10, 10,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Senior Manager', 20, 15,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","inventory.catalog.read","inventory.stock.read","dispatch.consignment_notes.read","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Administrator', 50, 10,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Manager', 50, 20,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["crm.leads.read","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Manager', 50, 20,
     '["inventory.catalog.manage","inventory.stock.manage","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","orders.orders.read","inventory.stock.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.users.update","identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb)
),
owner_batch AS (
    SELECT tenant_id, created_at
    FROM   identity.roles
    WHERE  name = 'Company Owner' AND is_system_default AND NOT is_super_admin AND NOT is_deleted
)
UPDATE identity.roles r
SET    hierarchy_level = CASE WHEN r.hierarchy_level = s.old_level THEN s.new_level ELSE r.hierarchy_level END,
       permissions     = CASE WHEN r.permissions @> s.old_perms AND s.old_perms @> r.permissions
                              THEN (SELECT jsonb_agg(DISTINCT e.v ORDER BY e.v)
                                    FROM jsonb_array_elements_text(r.permissions || s.add_perms) AS e(v))
                              ELSE r.permissions END
FROM   spec s, owner_batch ob
WHERE  r.name = s.name
  AND  r.tenant_id = ob.tenant_id
  AND  r.created_at = ob.created_at
  AND  NOT r.is_deleted
  AND  NOT r.is_system_default;

INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
SELECT gen_random_uuid(), ob.tenant_id, 'Team Lead', false, false, 30,
       '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.manage","tasks.work_items.read","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage","identity.meta.request_permission_elevation"]'::jsonb,
       ob.created_at
FROM   (SELECT tenant_id, created_at FROM identity.roles
        WHERE name = 'Company Owner' AND is_system_default AND NOT is_super_admin AND NOT is_deleted) ob
WHERE  NOT EXISTS (SELECT 1 FROM identity.roles x
                   WHERE x.tenant_id = ob.tenant_id AND x.name = 'Team Lead' AND NOT x.is_deleted);

-- Level 0 belongs to the system CompanyOwner role alone; the policy already
-- ranks every other level-0 role as 1 (see the header).
UPDATE identity.roles
SET    hierarchy_level = 1
WHERE  hierarchy_level = 0
  AND  NOT (name = 'Company Owner' AND is_system_default AND NOT is_super_admin);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

-- NOT reversed: the custom level-0 roles re-levelled to 1 in Up (the old level
-- cannot be told apart from a role that was always at 1).

DELETE FROM identity.roles r
USING  (SELECT tenant_id, created_at FROM identity.roles
        WHERE name = 'Company Owner' AND is_system_default AND NOT is_super_admin AND NOT is_deleted) ob
WHERE  r.name = 'Team Lead' AND r.tenant_id = ob.tenant_id AND r.created_at = ob.created_at
  AND  r.hierarchy_level = 30
  AND  NOT EXISTS (SELECT 1 FROM identity.role_assignments ra WHERE ra.role_id = r.id)
  AND  NOT EXISTS (SELECT 1 FROM identity.role_hierarchy_edges e WHERE e.child_role_id = r.id OR e.parent_role_id = r.id);

WITH spec(name, old_level, new_level, old_perms, add_perms) AS (
    VALUES
    ('Administrator', 10, 10,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Senior Manager', 20, 15,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","inventory.catalog.read","inventory.stock.read","dispatch.consignment_notes.read","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Administrator', 50, 10,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Office Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Manager', 50, 20,
     '["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Sales Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["crm.leads.read","crm.leads.manage","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Manager', 50, 20,
     '["inventory.catalog.manage","inventory.stock.manage","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.meta.request_permission_elevation"]'::jsonb),
    ('Purchase Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","orders.orders.read","inventory.stock.read","identity.meta.request_permission_elevation"]'::jsonb),
    ('Dispatch Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","dispatch.consignment_notes.read","dispatch.consignment_notes.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Manager', 50, 20,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","identity.users.update","identity.meta.request_permission_elevation"]'::jsonb),
    ('HR Executive', 50, 50,
     '["tasks.work_items.read","tasks.work_items.manage","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.meta.request_permission_elevation"]'::jsonb)
),
owner_batch AS (
    SELECT tenant_id, created_at
    FROM   identity.roles
    WHERE  name = 'Company Owner' AND is_system_default AND NOT is_super_admin AND NOT is_deleted
)
UPDATE identity.roles r
SET    hierarchy_level = CASE WHEN r.hierarchy_level = s.new_level THEN s.old_level ELSE r.hierarchy_level END,
       permissions     = CASE WHEN r.permissions @> (s.old_perms || s.add_perms) AND (s.old_perms || s.add_perms) @> r.permissions
                              THEN s.old_perms ELSE r.permissions END
FROM   spec s, owner_batch ob
WHERE  r.name = s.name
  AND  r.tenant_id = ob.tenant_id
  AND  r.created_at = ob.created_at
  AND  NOT r.is_deleted
  AND  NOT r.is_system_default;
-- +goose StatementEnd
