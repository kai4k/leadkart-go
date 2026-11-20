-- Owner ruling 2026-11-21: "SuperAdmin and Platform Team, Company Admin and
-- Company Owner are allowed to add users only." (ADR 0069 "Decided by the owner").
--
-- identity.users.create is held only at the Administrator tier or above (rank
-- <= 10: the Company Owner, the Administrator, Office Administrator and every
-- role in that band). New tenants already seed it that way (tenant.admin on the
-- three roles). This DATA migration brings EXISTING tenants in line: it removes
-- the permission from the places a member below the tier could hold it before
-- the ruling, so the stored state agrees with the rule the application now
-- enforces everywhere (the route gate, the command, capabilities).
--
-- It runs as the schema owner; every table here has FORCE row-level security, so
-- it sets app.is_platform for the transaction (as 20261119000001 does) and every
-- join to a row of another table also matches tenant_id, so one tenant's rows
-- never touch another's.
--
-- 1. Roles. A live role BELOW the tier (hierarchy_level > 10, or a stray level-0
--    role that is not a system role: authority.levelOf ranks it 50) loses
--    "identity.users.create" from its permission array. Roles at or above the
--    tier, the system roles and soft-deleted roles are untouched.
-- 2. Per-member overrides. A GRANTED override of the permission on a membership
--    whose rank is below the tier is deleted (a member's rank is the most senior
--    effective level of its LIVE roles; no live role is the user tier). A
--    REVOKED override of it stays: a revoke only narrows.
-- 3. Pending permission requests for it, from such a member, are cancelled
--    (state 'cancelled', decided_at, a decision_reason naming this migration).
--    Approved and denied history is left as it is.
--
-- It rotates no security stamp and writes no outbox row: nothing a member could
-- do changes (the application already treats the permission as inert below the
-- tier, in the token and in the capabilities), so existing sessions need no
-- refresh.
--
-- Cost and locks: one transaction (goose); ROW EXCLUSIVE on identity.roles,
-- identity.membership_permission_overrides, identity.permission_requests and the
-- audit table, no ACCESS EXCLUSIVE, no table rewrite. identity.roles and the
-- override table are scanned once each (small catalogues: roles per tenant, one
-- override row per grant; the filter on the permission name has no index and
-- needs none at this size); the per-membership "is it below the tier" test is a
-- NOT EXISTS probe over role_assignments (its primary key, membership_id first)
-- and roles (primary key), run only for rows that already matched the permission.
-- Only the rows that match are locked or written.
--
-- Audit rows are the rollback ledger (system actor, user_id NULL): one
-- common.audit_log_entry per changed role, per deleted override and per
-- cancelled request, with the before and after state.
--
-- Down undoes only what Up recorded: a role gets the permission back only while
-- its level and permissions still equal the recorded after state; a deleted
-- override is re-inserted only if its membership still exists in the same tenant
-- and the member has no row for that permission (ON CONFLICT DO NOTHING); a
-- cancelled request is re-opened only while it is still cancelled by this
-- migration and the member has no other pending request for the permission.
-- Everything Down restores is recorded as reverted, and a ledger entry already
-- reverted is not used again. The ledger is trusted only for rows with the system
-- actor (user_id NULL) and a tenant, and every join also matches the ledger
-- row's tenant, so a forged audit row can neither name another tenant's row nor
-- restore anything that Up did not remove from the recorded tenant. Not
-- restorable: roles edited since Up (left as they are) and the audit rows
-- (append-only, kept). Rolling this migration back re-grants a permission the
-- application ignores below the tier, so Down is only needed to return the data
-- to its earlier shape.

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

-- 1. Roles below the tier lose the permission.
WITH target AS (
    SELECT r.id, r.tenant_id, r.name, r.hierarchy_level AS lvl, r.permissions AS old_perms,
           (r.permissions - 'identity.users.create') AS new_perms
    FROM   identity.roles r
    WHERE  NOT r.is_deleted
      AND  (r.hierarchy_level > 10 OR (r.hierarchy_level = 0 AND NOT r.is_system_default))
      AND  jsonb_typeof(r.permissions) = 'array'
      AND  r.permissions @> '["identity.users.create"]'::jsonb
),
upd AS (
    UPDATE identity.roles r
    SET    permissions = t.new_perms
    FROM   target t
    WHERE  r.id = t.id AND r.tenant_id = t.tenant_id
    RETURNING r.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migrated', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'owner ruling 2026-11-21: identity.users.create is held only at the Administrator tier or above',
           'role_id', t.id, 'role_name', t.name,
           'before', jsonb_build_object('hierarchy_level', t.lvl, 'permissions', t.old_perms),
           'after',  jsonb_build_object('hierarchy_level', t.lvl, 'permissions', t.new_perms))
FROM   target t JOIN upd u ON u.id = t.id;

-- 2. Granted overrides on members below the tier are deleted.
WITH target AS (
    SELECT o.membership_id, o.tenant_id, o.permission_name, o.updated_at, o.expires_at
    FROM   identity.membership_permission_overrides o
    WHERE  o.permission_name = 'identity.users.create'
      AND  o.kind = 'granted'
      AND  NOT EXISTS (
               SELECT 1
               FROM   identity.role_assignments ra
               JOIN   identity.roles r ON r.id = ra.role_id AND r.tenant_id = ra.tenant_id
               WHERE  ra.membership_id = o.membership_id AND ra.tenant_id = o.tenant_id
                 AND  NOT r.is_deleted
                 AND  (CASE WHEN r.hierarchy_level = 0 AND NOT (r.is_system_default AND r.name = 'Company Owner')
                            THEN 50 ELSE r.hierarchy_level END) <= 10)
),
del AS (
    DELETE FROM identity.membership_permission_overrides o
    USING  target t
    WHERE  o.membership_id = t.membership_id AND o.tenant_id = t.tenant_id
      AND  o.permission_name = t.permission_name AND o.kind = 'granted'
    RETURNING o.membership_id, o.tenant_id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.membership_override.migrated', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'owner ruling 2026-11-21: identity.users.create is held only at the Administrator tier or above',
           'membership_id', t.membership_id, 'permission', t.permission_name,
           'before', jsonb_build_object('kind', 'granted', 'updated_at', t.updated_at, 'expires_at', t.expires_at))
FROM   target t JOIN del d ON d.membership_id = t.membership_id AND d.tenant_id = t.tenant_id;

-- 3. Pending requests for it from members below the tier are cancelled.
WITH target AS (
    SELECT p.id, p.tenant_id, p.requester_membership_id
    FROM   identity.permission_requests p
    WHERE  p.permission_constant = 'identity.users.create'
      AND  p.state = 'pending'
      AND  NOT EXISTS (
               SELECT 1
               FROM   identity.role_assignments ra
               JOIN   identity.roles r ON r.id = ra.role_id AND r.tenant_id = ra.tenant_id
               WHERE  ra.membership_id = p.requester_membership_id AND ra.tenant_id = p.tenant_id
                 AND  NOT r.is_deleted
                 AND  (CASE WHEN r.hierarchy_level = 0 AND NOT (r.is_system_default AND r.name = 'Company Owner')
                            THEN 50 ELSE r.hierarchy_level END) <= 10)
),
upd AS (
    UPDATE identity.permission_requests p
    SET    state = 'cancelled', decided_at = now(), updated_at = now(),
           decision_reason = 'cancelled by migration 20261121000001_identity_add_users_admin_tier_only: identity.users.create is held only at the Administrator tier or above (owner ruling 2026-11-21)'
    FROM   target t
    WHERE  p.id = t.id AND p.tenant_id = t.tenant_id AND p.state = 'pending'
    RETURNING p.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.permission_request.migrated', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'owner ruling 2026-11-21: identity.users.create is held only at the Administrator tier or above',
           'request_id', t.id, 'requester_membership_id', t.requester_membership_id,
           'before', jsonb_build_object('state', 'pending'),
           'after',  jsonb_build_object('state', 'cancelled'))
FROM   target t JOIN upd u ON u.id = t.id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

-- Requests Up cancelled are re-opened while they are still cancelled by this
-- migration and the member has no other pending request for the permission.
WITH ledger AS (
    SELECT DISTINCT ON (a.tenant_id, a.payload->>'request_id') a.tenant_id, a.occurred_at_utc, a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.permission_request.migrated'
      AND  a.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
      AND  a.user_id IS NULL AND a.tenant_id IS NOT NULL
      AND  NOT EXISTS (SELECT 1 FROM common.audit_log_entry x
                       WHERE x.action = 'identity.permission_request.migration_reverted'
                         AND x.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
                         AND x.user_id IS NULL AND x.tenant_id = a.tenant_id
                         AND x.payload->>'request_id' = a.payload->>'request_id'
                         AND x.occurred_at_utc > a.occurred_at_utc)
    ORDER  BY a.tenant_id, a.payload->>'request_id', a.occurred_at_utc DESC, a.id DESC
),
target AS (
    SELECT p.id, p.tenant_id, p.requester_membership_id
    FROM   ledger l
    JOIN   identity.permission_requests p ON p.id = (l.payload->>'request_id')::uuid AND p.tenant_id = l.tenant_id
    WHERE  p.state = 'cancelled'
      AND  p.permission_constant = 'identity.users.create'
      AND  p.requester_membership_id = (l.payload->>'requester_membership_id')::uuid
      AND  p.decision_reason LIKE 'cancelled by migration 20261121000001_identity_add_users_admin_tier_only:%'
      AND  p.approver_membership_id IS NULL
      AND  NOT EXISTS (SELECT 1 FROM identity.permission_requests q
                       WHERE q.tenant_id = p.tenant_id AND q.requester_membership_id = p.requester_membership_id
                         AND q.permission_constant = p.permission_constant AND q.state = 'pending')
),
upd AS (
    UPDATE identity.permission_requests p
    SET    state = 'pending', decided_at = NULL, decision_reason = NULL, updated_at = now()
    FROM   target t
    WHERE  p.id = t.id AND p.tenant_id = t.tenant_id
    RETURNING p.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.permission_request.migration_reverted', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'down migration: request re-opened',
           'request_id', t.id, 'requester_membership_id', t.requester_membership_id)
FROM   target t JOIN upd u ON u.id = t.id;

-- Overrides Up deleted come back for a member that still exists in the same
-- tenant and has no row for the permission.
WITH ledger AS (
    SELECT DISTINCT ON (a.tenant_id, a.payload->>'membership_id') a.tenant_id, a.occurred_at_utc, a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.membership_override.migrated'
      AND  a.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
      AND  a.user_id IS NULL AND a.tenant_id IS NOT NULL
      AND  NOT EXISTS (SELECT 1 FROM common.audit_log_entry x
                       WHERE x.action = 'identity.membership_override.migration_reverted'
                         AND x.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
                         AND x.user_id IS NULL AND x.tenant_id = a.tenant_id
                         AND x.payload->>'membership_id' = a.payload->>'membership_id'
                         AND x.occurred_at_utc > a.occurred_at_utc)
    ORDER  BY a.tenant_id, a.payload->>'membership_id', a.occurred_at_utc DESC, a.id DESC
),
ins AS (
    INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at, expires_at)
    SELECT m.id, l.payload->>'permission', 'granted', m.tenant_id,
           (l.payload #>> '{before,updated_at}')::timestamptz,
           (l.payload #>> '{before,expires_at}')::timestamptz
    FROM   ledger l
    JOIN   identity.tenant_memberships m ON m.id = (l.payload->>'membership_id')::uuid AND m.tenant_id = l.tenant_id
    WHERE  l.payload->>'permission' = 'identity.users.create'
    ON CONFLICT (membership_id, permission_name) DO NOTHING
    RETURNING membership_id, tenant_id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.membership_override.migration_reverted', NULL, i.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'down migration: override restored',
           'membership_id', i.membership_id, 'permission', 'identity.users.create')
FROM   ins i;

-- Roles Up changed go back to the recorded before state, but only while they
-- still equal the recorded after state (a role edited since is left alone).
WITH ledger AS (
    SELECT DISTINCT ON (a.tenant_id, a.payload->>'role_id') a.tenant_id, a.occurred_at_utc, a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.role.migrated'
      AND  a.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
      AND  a.user_id IS NULL AND a.tenant_id IS NOT NULL
      AND  NOT EXISTS (SELECT 1 FROM common.audit_log_entry x
                       WHERE x.action = 'identity.role.migration_reverted'
                         AND x.payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only'
                         AND x.user_id IS NULL AND x.tenant_id = a.tenant_id
                         AND x.payload->>'role_id' = a.payload->>'role_id'
                         AND x.occurred_at_utc > a.occurred_at_utc)
    ORDER  BY a.tenant_id, a.payload->>'role_id', a.occurred_at_utc DESC, a.id DESC
),
target AS (
    SELECT r.id, r.tenant_id, r.name, r.hierarchy_level AS lvl,
           r.permissions AS cur_perms, l.payload #> '{before,permissions}' AS old_perms
    FROM   ledger l
    JOIN   identity.roles r ON r.id = (l.payload->>'role_id')::uuid AND r.tenant_id = l.tenant_id
    WHERE  r.hierarchy_level = (l.payload #>> '{after,hierarchy_level}')::int
      AND  r.permissions @> (l.payload #> '{after,permissions}')
      AND  (l.payload #> '{after,permissions}') @> r.permissions
),
upd AS (
    UPDATE identity.roles r
    SET    permissions = t.old_perms
    FROM   target t
    WHERE  r.id = t.id AND r.tenant_id = t.tenant_id
    RETURNING r.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.role.migration_reverted', NULL, t.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000001_identity_add_users_admin_tier_only',
           'reason', 'down migration: role restored to its state before the migration',
           'role_id', t.id, 'role_name', t.name,
           'before', jsonb_build_object('hierarchy_level', t.lvl, 'permissions', t.cur_perms),
           'after',  jsonb_build_object('hierarchy_level', t.lvl, 'permissions', t.old_perms))
FROM   target t JOIN upd u ON u.id = t.id;
-- +goose StatementEnd
