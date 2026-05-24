-- LeadKart Go — Force RLS on all tenant-scoped tables (security hardening)
--
-- Surfaced by the architectural fitness-function suite
-- (TestArch_EveryTenantTableHasRLSAndForce in internal/architecture/
-- multi_tenancy_arch_test.go). 15 tables enabled RLS without
-- pairing FORCE — a security-class gap because Postgres exempts
-- table owners + superusers from RLS unless FORCE is set explicitly
-- (PG docs §5.8 + §13.3.2). In some deployment shapes the
-- application connection runs as the table owner (sqlc-generated
-- migration code; tests using the goose role), which means the
-- RLS predicate is silently bypassed.
--
-- Every policy on these tables already includes the
-- `app.is_platform()` bypass clause for legitimate platform-operator
-- access, so applying FORCE is behavior-preserving for the canonical
-- access paths (TxScopeTenant + TxScopePlatform) while closing the
-- owner-bypass hole.
--
-- Tables affected:
--   identity.membership_permission_overrides
--   identity.outbox
--   identity.role_assignments
--   identity.role_hierarchy_edges
--   identity.roles
--   identity.tenant_memberships
--   inventory.batches
--   inventory.outbox
--   inventory.products
--   inventory.stock_movements
--   platform.lead_credits
--   platform.outbox
--   platform.platform_leads
--   platform.unverified_contacts
--   platform.verification_calls
--
-- Reference: PG docs §5.8 "Row Security Policies" — "Superusers and
-- roles with the BYPASSRLS attribute always bypass the row security
-- system when accessing a table. Table owners normally bypass row
-- security as well, though a table owner can choose to be subject to
-- row security with ALTER TABLE ... FORCE ROW LEVEL SECURITY."

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.membership_permission_overrides FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.outbox                          FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.role_assignments                FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.role_hierarchy_edges            FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.roles                           FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.tenant_memberships              FORCE ROW LEVEL SECURITY;

ALTER TABLE inventory.batches                        FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.outbox                         FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.products                       FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.stock_movements                FORCE ROW LEVEL SECURITY;

ALTER TABLE platform.lead_credits                    FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.outbox                          FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.platform_leads                  FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.unverified_contacts             FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.verification_calls              FORCE ROW LEVEL SECURITY;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.membership_permission_overrides NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.outbox                          NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.role_assignments                NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.role_hierarchy_edges            NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.roles                           NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.tenant_memberships              NO FORCE ROW LEVEL SECURITY;

ALTER TABLE inventory.batches                        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.outbox                         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.products                       NO FORCE ROW LEVEL SECURITY;
ALTER TABLE inventory.stock_movements                NO FORCE ROW LEVEL SECURITY;

ALTER TABLE platform.lead_credits                    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.outbox                          NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.platform_leads                  NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.unverified_contacts             NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform.verification_calls              NO FORCE ROW LEVEL SECURITY;

-- +goose StatementEnd
