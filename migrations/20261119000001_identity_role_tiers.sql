-- ADR 0069 (H1/H5) — rank tiers + manager-tier daily-work grants for EXISTING
-- tenants' seeded roles. New tenants receive the same shape from
-- seed.DefaultRoleCatalog at registration.
--
-- This is a DATA migration over existing tenants. It runs as the schema owner,
-- so row-level security does not apply: every statement joins on tenant_id and
-- touches one tenant's rows only through that join. It is idempotent (a second
-- Up changes nothing) and written not to fail on real-world data shapes
-- (non-array permissions, case-variant names, soft-deleted rows, orphan edges).
--
-- 1. Level 0 belongs to the system CompanyOwner role alone. A NON-system role
--    stored at level 0 (POST /roles used to decode an omitted level as 0) is
--    moved to the user tier, 50, failing closed; authority.levelOf ranks it 50
--    too. The bootstrap SuperAdmin system role moves 0 -> 1, matching
--    cmd/bootstrap. Other system-default roles are left alone.
-- 2. A seeded role is upgraded ONLY when it is fully pristine: its name is a
--    DefaultRoleCatalog name, it is live and not system-default, it belongs to
--    the tenant's seed batch (same created_at as the system 'Company Owner'
--    role: ApplyDefaultRoles stamps a batch with one `now`), AND its level AND
--    its permission SET still equal the old seed values. Level and permissions
--    change together or not at all.
--    - Office Administrator is NOT upgraded: no silent promotion of clerk-grade
--      holders to approvers of everything. It keeps level and permissions.
--    - Administrator keeps level 10 and gains the read bundle plus the
--      elevation request permission; it does NOT gain tenant.admin (Q2).
-- 3. 'Team Lead' (level 30) is inserted for a tenant only when no live role
--    there is named 'team lead' (case-insensitive, trimmed) AND every live
--    seeded-manager-named role is, after step 2, strictly more senior than 30.
--    The row id is derived from the tenant id (marker, see Down) and created_at
--    is this migration's now().
-- 4. Level changes can leave an active role edge whose parent is no longer
--    strictly more senior than its child. Such edges (touching a role this run
--    changed) are soft-removed (removed_at, removal_reason), the table's own
--    history mechanism; nothing is deleted.
-- 5. Audit: one row per changed or created role and per removed edge goes into
--    common.audit_log_entry (the table the audit middleware writes and the
--    tenant audit endpoint reads): system actor (user_id NULL), the tenant, a
--    payload naming this migration, the reason and the before/after state.
--    Those rows are also Down's ledger of what Up did.
--
-- Down undoes only what Up recorded: a role goes back to its recorded before
-- state only while it still equals the recorded after state; a created Team
-- Lead is deleted only while it is unchanged and has no assignment or active
-- edge; a removed edge is restored only while its child has no active edge and
-- the parent is again strictly more senior. Not restorable: roles changed
-- after Up (left as they are), and audit rows (append-only, kept).

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

-- 1. Re-level level-0 roles that are not the CompanyOwner system role.
WITH target AS (
    SELECT r.id, r.tenant_id, r.name, r.hierarchy_level AS old_level, r.permissions,
           CASE WHEN r.is_system_default THEN 1 ELSE 50 END AS new_level
    FROM   identity.roles r
    WHERE  r.hierarchy_level = 0
      AND  (NOT r.is_system_default OR r.is_super_admin)
),
upd AS (
    UPDATE identity.roles r
    SET    hierarchy_level = t.new_level
    FROM   target t
    WHERE  r.id = t.id AND r.tenant_id = t.tenant_id
    RETURNING r.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migrated', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'ADR 0069: level 0 is the owner tier only; legacy level-0 role re-levelled',
           'role_id', t.id, 'role_name', t.name,
           'before', jsonb_build_object('hierarchy_level', t.old_level, 'permissions', t.permissions),
           'after',  jsonb_build_object('hierarchy_level', t.new_level, 'permissions', t.permissions))
FROM   target t JOIN upd u ON u.id = t.id;

-- 2. Upgrade pristine seeded roles (level and permissions together).
WITH spec(name, old_level, new_level, old_perms, add_perms) AS (
    VALUES
    -- Administrator gains the READ bundle only ("Admin sees everything"), never
    -- tenant.admin (ADR 0069 Q2): users/roles/tenants views, every CRM lead,
    -- inventory, dispatch and order read, lead credits and marketplace browse.
    ('Administrator', 10, 10,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.tenants.view","crm.leads.read","crm.leads.read_all","inventory.catalog.read","inventory.stock.read","dispatch.consignment_notes.read","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","platform.lead_credits.read","platform.marketplace.browse","identity.meta.request_permission_elevation"]'::jsonb),
    ('Senior Manager', 20, 15,
     '["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]'::jsonb,
     '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.read_all","crm.leads.assign","crm.leads.manage","inventory.catalog.read","inventory.stock.read","dispatch.consignment_notes.read","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","identity.meta.request_permission_elevation"]'::jsonb),
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
    -- HR Manager keeps identity.users.update (reporting lines): the manager
    -- commands decide through the authority policy (rank, ADR 0069 H4), so the
    -- permission no longer lets an HR Manager appoint itself over a peer or senior.
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
),
target AS (
    SELECT r.id, r.tenant_id, r.name, r.hierarchy_level AS old_level, r.permissions AS old_perms,
           s.new_level,
           (SELECT jsonb_agg(DISTINCT e.v ORDER BY e.v)
            FROM   jsonb_array_elements_text(s.old_perms || s.add_perms) AS e(v)) AS new_perms
    FROM   identity.roles r
    JOIN   owner_batch ob ON ob.tenant_id = r.tenant_id AND ob.created_at = r.created_at
    JOIN   spec s         ON s.name = r.name
    WHERE  NOT r.is_deleted
      AND  NOT r.is_system_default
      AND  r.hierarchy_level = s.old_level
      AND  jsonb_typeof(r.permissions) = 'array'
      AND  r.permissions @> s.old_perms AND s.old_perms @> r.permissions
),
upd AS (
    UPDATE identity.roles r
    SET    hierarchy_level = t.new_level, permissions = t.new_perms
    FROM   target t
    WHERE  r.id = t.id AND r.tenant_id = t.tenant_id
    RETURNING r.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migrated', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'ADR 0069: pristine seeded role upgraded to its rank tier and daily-work grants',
           'role_id', t.id, 'role_name', t.name,
           'before', jsonb_build_object('hierarchy_level', t.old_level, 'permissions', t.old_perms),
           'after',  jsonb_build_object('hierarchy_level', t.new_level, 'permissions', t.new_perms))
FROM   target t JOIN upd u ON u.id = t.id;

-- 3. Team Lead, only where it cannot outrank the tenant's managers.
WITH owner_roles AS (
    SELECT tenant_id
    FROM   identity.roles
    WHERE  name = 'Company Owner' AND is_system_default AND NOT is_super_admin AND NOT is_deleted
),
eligible AS (
    SELECT o.tenant_id, md5(o.tenant_id::text || ':20261119000001_identity_role_tiers:team_lead') AS h
    FROM   owner_roles o
    WHERE  NOT EXISTS (SELECT 1 FROM identity.roles x
                       WHERE x.tenant_id = o.tenant_id AND NOT x.is_deleted
                         AND lower(btrim(x.name)) = 'team lead')
      AND  NOT EXISTS (SELECT 1 FROM identity.roles x
                       WHERE x.tenant_id = o.tenant_id AND NOT x.is_deleted
                         AND lower(btrim(x.name)) IN ('senior manager', 'sales manager', 'purchase manager', 'dispatch manager', 'hr manager')
                         AND x.hierarchy_level >= 30)
),
ins AS (
    INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
    SELECT (substr(e.h, 1, 12) || '5' || substr(e.h, 14, 3) || '8' || substr(e.h, 18, 15))::uuid,
           e.tenant_id, 'Team Lead', false, false, 30,
           '["identity.users.view","identity.roles.view","identity.roles.approve","crm.leads.read","crm.leads.manage","tasks.work_items.read","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage","identity.meta.request_permission_elevation"]'::jsonb,
           now()
    FROM   eligible e
    ON CONFLICT (id) DO NOTHING
    RETURNING id, tenant_id, name, hierarchy_level, permissions
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.created', NULL, i.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'ADR 0069: seeded Team Lead role added to an existing tenant',
           'role_id', i.id, 'role_name', i.name,
           'after', jsonb_build_object('hierarchy_level', i.hierarchy_level, 'permissions', i.permissions))
FROM   ins i;

-- 4. Soft-remove active edges this run made invalid (parent not strictly more
--    senior than child).
WITH changed AS (
    SELECT DISTINCT (a.payload->>'role_id')::uuid AS role_id
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.role.migrated'
      AND  a.payload->>'migration' = '20261119000001_identity_role_tiers'
      AND  a.occurred_at_utc = now()
),
bad AS (
    SELECT e.id, e.tenant_id, e.parent_role_id, e.child_role_id
    FROM   identity.role_hierarchy_edges e
    JOIN   identity.roles p ON p.tenant_id = e.tenant_id AND p.id = e.parent_role_id
    JOIN   identity.roles c ON c.tenant_id = e.tenant_id AND c.id = e.child_role_id
    WHERE  e.removed_at IS NULL
      AND  p.hierarchy_level >= c.hierarchy_level
      AND  (p.id IN (SELECT role_id FROM changed) OR c.id IN (SELECT role_id FROM changed))
),
upd AS (
    UPDATE identity.role_hierarchy_edges e
    SET    removed_at = now(),
           removal_reason = 'removed by migration 20261119000001_identity_role_tiers: parent is no longer more senior than child'
    FROM   bad b
    WHERE  e.id = b.id AND e.tenant_id = b.tenant_id
    RETURNING e.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role_edge.removed', NULL, b.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'ADR 0069: role levels changed, parent is no longer strictly more senior than child',
           'edge_id', b.id, 'parent_role_id', b.parent_role_id, 'child_role_id', b.child_role_id)
FROM   bad b JOIN upd u ON u.id = b.id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

-- Roles Up changed go back to the recorded before state, but only while they
-- still equal the recorded after state (a role edited since is left alone).
WITH ledger AS (
    SELECT DISTINCT ON (a.payload->>'role_id') a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.role.migrated' AND a.payload->>'migration' = '20261119000001_identity_role_tiers'
    ORDER  BY a.payload->>'role_id', a.occurred_at_utc DESC, a.id DESC
),
target AS (
    SELECT r.id, r.tenant_id, r.name,
           r.hierarchy_level AS cur_level, r.permissions AS cur_perms,
           (l.payload #>> '{before,hierarchy_level}')::int AS old_level,
           l.payload #> '{before,permissions}' AS old_perms
    FROM   ledger l
    JOIN   identity.roles r ON r.id = (l.payload->>'role_id')::uuid
    WHERE  r.hierarchy_level = (l.payload #>> '{after,hierarchy_level}')::int
      AND  r.permissions @> (l.payload #> '{after,permissions}')
      AND  (l.payload #> '{after,permissions}') @> r.permissions
),
upd AS (
    UPDATE identity.roles r
    SET    hierarchy_level = t.old_level, permissions = t.old_perms
    FROM   target t
    WHERE  r.id = t.id AND r.tenant_id = t.tenant_id
    RETURNING r.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migration_reverted', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'down migration: role restored to its state before the migration',
           'role_id', t.id, 'role_name', t.name,
           'before', jsonb_build_object('hierarchy_level', t.cur_level, 'permissions', t.cur_perms),
           'after',  jsonb_build_object('hierarchy_level', t.old_level, 'permissions', t.old_perms))
FROM   target t JOIN upd u ON u.id = t.id;

-- Team Lead roles Up created: deleted only while unchanged, unassigned and
-- without an active edge. Anything else stays.
WITH ledger AS (
    SELECT DISTINCT ON (a.payload->>'role_id') a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.role.created' AND a.payload->>'migration' = '20261119000001_identity_role_tiers'
    ORDER  BY a.payload->>'role_id', a.occurred_at_utc DESC, a.id DESC
),
del AS (
    DELETE FROM identity.roles r
    USING  ledger l
    WHERE  r.id = (l.payload->>'role_id')::uuid
      AND  r.hierarchy_level = (l.payload #>> '{after,hierarchy_level}')::int
      AND  r.permissions @> (l.payload #> '{after,permissions}')
      AND  (l.payload #> '{after,permissions}') @> r.permissions
      AND  NOT EXISTS (SELECT 1 FROM identity.role_assignments ra WHERE ra.tenant_id = r.tenant_id AND ra.role_id = r.id)
      AND  NOT EXISTS (SELECT 1 FROM identity.role_hierarchy_edges e
                       WHERE e.tenant_id = r.tenant_id AND e.removed_at IS NULL
                         AND (e.child_role_id = r.id OR e.parent_role_id = r.id))
    RETURNING r.id, r.tenant_id, r.name
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migration_deleted', NULL, d.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'down migration: role created by the migration removed',
           'role_id', d.id, 'role_name', d.name)
FROM   del d;

-- Edges Up removed come back when they are valid again and the child has no
-- active edge of its own.
WITH ledger AS (
    SELECT DISTINCT ON (a.payload->>'child_role_id') a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.role_edge.removed' AND a.payload->>'migration' = '20261119000001_identity_role_tiers'
    ORDER  BY a.payload->>'child_role_id', a.occurred_at_utc DESC, a.id DESC
),
restorable AS (
    SELECT e.id, e.tenant_id, e.parent_role_id, e.child_role_id
    FROM   ledger l
    JOIN   identity.role_hierarchy_edges e ON e.id = (l.payload->>'edge_id')::uuid
    JOIN   identity.roles p ON p.tenant_id = e.tenant_id AND p.id = e.parent_role_id
    JOIN   identity.roles c ON c.tenant_id = e.tenant_id AND c.id = e.child_role_id
    WHERE  e.removed_at IS NOT NULL
      AND  e.removal_reason = 'removed by migration 20261119000001_identity_role_tiers: parent is no longer more senior than child'
      AND  NOT p.is_deleted AND NOT c.is_deleted
      AND  p.hierarchy_level < c.hierarchy_level
      AND  NOT EXISTS (SELECT 1 FROM identity.role_hierarchy_edges a
                       WHERE a.tenant_id = e.tenant_id AND a.child_role_id = e.child_role_id AND a.removed_at IS NULL)
),
upd AS (
    UPDATE identity.role_hierarchy_edges e
    SET    removed_at = NULL, removed_by_membership_id = NULL, removal_reason = NULL
    FROM   restorable x
    WHERE  e.id = x.id AND e.tenant_id = x.tenant_id
    RETURNING e.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role_edge.restored', NULL, x.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261119000001_identity_role_tiers',
           'reason', 'down migration: role edge restored',
           'edge_id', x.id, 'parent_role_id', x.parent_role_id, 'child_role_id', x.child_role_id)
FROM   restorable x JOIN upd u ON u.id = x.id;
-- +goose StatementEnd
