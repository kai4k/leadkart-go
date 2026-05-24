-- LeadKart Go — Wave 1.5 audit-chain backfill across tenant-scoped tables.
--
-- ADR 0027 (outbox doubles as audit log) + Wave 1.5 audit-chain
-- (migration 20260507000008_audit_chain_replaces_admin_email.sql).
--
-- Closes the audit-chain gap that the arch-test
-- `TestArch_AuditChainColumnsOnTenantTables` flagged: every tenant-
-- scoped mutable aggregate SHOULD carry `created_by_membership_id uuid`
-- so the row's authorship is part of the per-row state (Stripe / Plaid
-- / Salesforce shape — the "who created this" question is answered
-- declaratively, not reconstructed from event streams).
--
-- Tables touched (tenant-scoped + mutable + NOT excluded):
--   identity.roles
--   identity.role_assignments
--   inventory.products
--   inventory.batches
--
-- Tables intentionally NOT touched (per task spec):
--   identity.tenants                          → global aggregate
--   identity.persons                          → global identity
--   identity.refresh_token_families           → session infra
--   identity.refresh_tokens                   → session infra
--   identity.membership_permission_overrides  → permission* family
--   identity.permission_requests              → permission* family
--   identity.role_hierarchy_edges             → rolehierarchy* family
--                                               (already carries
--                                                established_by_membership_id)
--   identity.tenant_memberships               → already shipped
--                                                created_by_membership_id
--                                                in 20260507000008
--   platform.unverified_contacts              → already shipped
--                                                created_by_membership_id
--                                                in 20260601000001
--   platform.verification_calls               → append-only ledger
--   platform.platform_leads                   → marketplace global
--   platform.lead_credits                     → balance aggregate
--   inventory.stock_movements                 → event-stream aggregate
--   *.outbox, *.processed_messages,           → infra
--   buildingblocks.*, identity.auth_routing
--   identity.persons (credential / lockout / etc.)
--
-- Discipline:
--   - `created_by_membership_id` is uuid NULL. NULL = system-bootstrapped
--     (seed catalogue, default-role seed, etc.) — same semantics as
--     20260507000008.
--   - NO foreign-key constraint to identity.tenant_memberships at this
--     time — cross-schema FK to identity from inventory/platform is a
--     future change (the composite-FK shape used in 20260507000008
--     requires tenant_id + a candidate-key on (id, tenant_id) which
--     these tables would need to declare on their own membership refs).
--     Tracked as a follow-up; the column itself is forward-only audit
--     metadata + can sit naked until then.
--   - NO backfill of historical rows. Audit-chain is forward-only;
--     existing rows stay NULL (system-bootstrapped per the canonical
--     interpretation).
--   - `ADD COLUMN IF NOT EXISTS` makes re-runs a no-op (Postgres 9.6+).

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.roles
    ADD COLUMN IF NOT EXISTS created_by_membership_id uuid NULL;

COMMENT ON COLUMN identity.roles.created_by_membership_id IS
    'Audit chain — Membership that created this role. NULL = system-bootstrapped (DefaultRoleCatalog seed, SuperAdmin seed). Wave 1.5 ADR 0027.';

ALTER TABLE identity.role_assignments
    ADD COLUMN IF NOT EXISTS created_by_membership_id uuid NULL;

COMMENT ON COLUMN identity.role_assignments.created_by_membership_id IS
    'Audit chain — Membership that assigned this role. NULL = system-bootstrapped (TenantOnboarding auto-CompanyOwner assignment, SuperAdmin seed). Wave 1.5 ADR 0027.';

ALTER TABLE inventory.products
    ADD COLUMN IF NOT EXISTS created_by_membership_id uuid NULL;

COMMENT ON COLUMN inventory.products.created_by_membership_id IS
    'Audit chain — Membership that created this product. NULL = system-bootstrapped (seed / import). Wave 1.5 ADR 0027.';

ALTER TABLE inventory.batches
    ADD COLUMN IF NOT EXISTS created_by_membership_id uuid NULL;

COMMENT ON COLUMN inventory.batches.created_by_membership_id IS
    'Audit chain — Membership that created this batch. NULL = system-bootstrapped (seed / import). Wave 1.5 ADR 0027.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE inventory.batches          DROP COLUMN IF EXISTS created_by_membership_id;
ALTER TABLE inventory.products         DROP COLUMN IF EXISTS created_by_membership_id;
ALTER TABLE identity.role_assignments  DROP COLUMN IF EXISTS created_by_membership_id;
ALTER TABLE identity.roles             DROP COLUMN IF EXISTS created_by_membership_id;

-- +goose StatementEnd
