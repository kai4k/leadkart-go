-- Owner ruling 2026-11-21: "If a Company is not Active no one can login from that
-- org." (ADR 0069 "Decided by the owner"). Only an ACTIVE company's members sign in.
--
-- Until now a newly registered company was created `pending` and nothing ever
-- activated it, and `pending` members signed in like any other. Every company
-- registered so far is therefore `pending` AND fully in use. The application now
-- creates companies active at registration (the controller default, ADR 0069), and
-- this DATA migration brings the EXISTING rows in line: every `pending` tenant
-- becomes `active`, activated_at = the migration time. Without it, the ruling would
-- lock out every company registered before it.
--
-- Nothing else changes: suspended, pending_deletion and deleted tenants are left
-- exactly as they are (the ruling already refuses them), `active` tenants are not
-- touched, and no schema object is created or altered (status already allows
-- 'active'). No refresh family, security stamp or outbox row is written: the
-- members of a pending company could sign in until now, and now they still can.
--
-- identity.tenants has no row-level security; the audit table has FORCE RLS, so the
-- transaction sets app.is_platform (as 20261119000001 and 20261121000001 do).
--
-- Cost and locks: one transaction (goose); ROW EXCLUSIVE on identity.tenants and
-- common.audit_log_entry, no ACCESS EXCLUSIVE, no rewrite. One scan of identity.tenants
-- (a directory with one row per company; idx_tenants_status already serves the
-- `status = 'pending'` filter); only the pending rows are locked and written.
--
-- Audit rows are the rollback ledger (system actor, user_id NULL): one
-- common.audit_log_entry per tenant changed, in that tenant (tenant_id = the tenant's
-- own id), with the before and after state.
--
-- Down undoes only what Up recorded: a tenant goes back to `pending` (activated_at
-- NULL, the recorded before state) only while it is still `active` with the
-- activated_at Up wrote, i.e. nobody suspended, marked or re-activated it since; a
-- tenant that was already active before Up, or registered after it, has no ledger row
-- and is never touched. A ledger entry already reverted is not used again, and the
-- ledger is trusted only for rows with the system actor (user_id NULL) whose
-- tenant_id is the tenant they name, so a forged audit row restores nothing.
-- Rolling this back returns the data to its earlier shape; the previous application
-- code let a pending company sign in, the current code does not, so run Down only
-- together with the code that goes with it.

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

WITH target AS (
    SELECT t.id, t.slug, t.activated_at AS old_activated_at, now() AS new_activated_at
    FROM   identity.tenants t
    WHERE  t.status = 'pending'
),
upd AS (
    UPDATE identity.tenants t
    SET    status = 'active', activated_at = g.new_activated_at
    FROM   target g
    WHERE  t.id = g.id AND t.status = 'pending'
    RETURNING t.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.tenant.migrated', NULL, g.id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000002_identity_pending_tenants_active',
           'reason', 'owner ruling 2026-11-21: only an active company''s members sign in; a registered company is created active',
           'tenant_id', g.id, 'slug', g.slug,
           'before', jsonb_build_object('status', 'pending', 'activated_at', g.old_activated_at),
           'after',  jsonb_build_object('status', 'active',  'activated_at', g.new_activated_at))
FROM   target g JOIN upd u ON u.id = g.id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

WITH ledger AS (
    SELECT DISTINCT ON (a.tenant_id) a.tenant_id, a.occurred_at_utc, a.payload
    FROM   common.audit_log_entry a
    WHERE  a.action = 'identity.tenant.migrated'
      AND  a.payload->>'migration' = '20261121000002_identity_pending_tenants_active'
      AND  a.user_id IS NULL AND a.tenant_id IS NOT NULL
      AND  a.payload->>'tenant_id' = a.tenant_id::text
      AND  NOT EXISTS (SELECT 1 FROM common.audit_log_entry x
                       WHERE x.action = 'identity.tenant.migration_reverted'
                         AND x.payload->>'migration' = '20261121000002_identity_pending_tenants_active'
                         AND x.user_id IS NULL AND x.tenant_id = a.tenant_id
                         AND x.occurred_at_utc > a.occurred_at_utc)
    ORDER  BY a.tenant_id, a.occurred_at_utc DESC, a.id DESC
),
target AS (
    SELECT t.id, t.slug
    FROM   ledger l
    JOIN   identity.tenants t ON t.id = l.tenant_id
    WHERE  t.status = 'active'
      AND  t.activated_at = (l.payload #>> '{after,activated_at}')::timestamptz
),
upd AS (
    UPDATE identity.tenants t
    SET    status = 'pending', activated_at = NULL
    FROM   target g
    WHERE  t.id = g.id AND t.status = 'active'
    RETURNING t.id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.tenant.migration_reverted', NULL, g.id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261121000002_identity_pending_tenants_active',
           'reason', 'down migration: tenant restored to pending',
           'tenant_id', g.id, 'slug', g.slug)
FROM   target g JOIN upd u ON u.id = g.id;
-- +goose StatementEnd
