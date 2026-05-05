-- LeadKart Go — Identity module initial schema.
-- Per LeadKart .NET multi-tenancy.md "Identity model" + ADR 0006/0027.
--
-- Tenant-scoping rules (canonical reference for table-by-table verdicts):
--
--   identity.tenants                 → NOT tenant-scoped (each row IS a tenant)
--   identity.persons                 → NOT tenant-scoped (global identity)
--   identity.tenant_memberships      → tenant-scoped, RLS + FORCE
--   identity.refresh_token_families  → NOT tenant-scoped (session infrastructure)
--   identity.refresh_tokens          → NOT tenant-scoped (session infrastructure)
--   identity.auth_routing            → NOT tenant-scoped (cross-tenant email→tenant index)
--   identity.outbox                  → tenant-scoped, RLS + FORCE (per ADR 0027)

-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS identity;
COMMENT ON SCHEMA identity IS 'LeadKart Identity module — Tenants, Persons, Memberships, RefreshTokenFamilies, AuthRouting.';

-- ============================================================================
-- identity.tenants
-- Each row IS a tenant. Globally unique slug; no RLS (would block tenant
-- self-resolution at login).
-- ============================================================================

CREATE TABLE identity.tenants (
    id              uuid        PRIMARY KEY,
    slug            text        NOT NULL UNIQUE,
    legal_name      text        NOT NULL CHECK (length(legal_name) > 0 AND length(legal_name) <= 200),
    display_name    text        NOT NULL CHECK (length(display_name) > 0 AND length(display_name) <= 200),
    admin_email     text        NOT NULL CHECK (length(admin_email) <= 254),
    status          text        NOT NULL CHECK (status IN ('pending', 'active', 'suspended')),
    created_at      timestamptz NOT NULL,
    activated_at    timestamptz NULL,
    suspended_at    timestamptz NULL
);
CREATE INDEX idx_tenants_status ON identity.tenants (status) WHERE status != 'suspended';
COMMENT ON TABLE identity.tenants IS 'Tenant aggregate root. Each row IS a tenant; not tenant-scoped (no RLS).';

-- ============================================================================
-- identity.persons
-- Global identity. Email globally unique system-wide. NOT tenant-scoped.
-- ============================================================================

CREATE TABLE identity.persons (
    id               uuid        PRIMARY KEY,
    email            text        NOT NULL UNIQUE CHECK (length(email) <= 254),
    first_name       text        NOT NULL CHECK (length(first_name) > 0 AND length(first_name) <= 100),
    last_name        text        NOT NULL CHECK (length(last_name) > 0 AND length(last_name) <= 100),
    password_hash    text        NOT NULL,
    security_stamp   uuid        NOT NULL,
    is_active        boolean     NOT NULL DEFAULT true,
    is_anonymised    boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL,
    anonymised_at    timestamptz NULL
);
CREATE INDEX idx_persons_active ON identity.persons (id) WHERE is_active AND NOT is_anonymised;
COMMENT ON TABLE identity.persons IS 'Person aggregate. Global identity (Auth0/Entra ID pattern). NOT tenant-scoped.';

-- ============================================================================
-- identity.tenant_memberships
-- Per-tenant junction. Tenant-scoped + RLS + FORCE.
-- DB-enforced invariant: at most one Active Membership per Person.
-- ============================================================================

CREATE TABLE identity.tenant_memberships (
    id          uuid        PRIMARY KEY,
    person_id   uuid        NOT NULL REFERENCES identity.persons(id),
    tenant_id   uuid        NOT NULL REFERENCES identity.tenants(id),
    status      text        NOT NULL CHECK (status IN ('active', 'inactive')),
    joined_at   timestamptz NOT NULL,
    left_at     timestamptz NULL
);

-- (person_id, tenant_id) uniqueness — one Membership per Person per Tenant.
CREATE UNIQUE INDEX uq_memberships_person_tenant
    ON identity.tenant_memberships (person_id, tenant_id);

-- The single-Active-Membership invariant per LeadKart .NET multi-tenancy.md.
-- A Person can have at most ONE Active Membership system-wide at any time.
CREATE UNIQUE INDEX uq_memberships_person_active
    ON identity.tenant_memberships (person_id)
    WHERE status = 'active';

CREATE INDEX idx_memberships_tenant ON identity.tenant_memberships (tenant_id);

ALTER TABLE identity.tenant_memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.tenant_memberships FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_memberships_select ON identity.tenant_memberships
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tenant_memberships_modify ON identity.tenant_memberships
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tenant_memberships_insert ON identity.tenant_memberships
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tenant_memberships_delete ON identity.tenant_memberships
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE identity.tenant_memberships IS 'Per-tenant junction (Person ↔ Tenant). Tenant-scoped, FORCE RLS. Single-Active-Membership invariant via partial unique index.';

-- ============================================================================
-- identity.refresh_token_families + refresh_tokens
-- Session-management infrastructure. NOT tenant-scoped (Auth0/Okta canon —
-- token-hash uniqueness is the load-bearing isolation). TenantID carried
-- as data column for context.
-- ============================================================================

CREATE TABLE identity.refresh_token_families (
    id            uuid        PRIMARY KEY,
    person_id     uuid        NOT NULL REFERENCES identity.persons(id),
    tenant_id     uuid        NOT NULL REFERENCES identity.tenants(id),
    device_label  text        NOT NULL CHECK (length(device_label) > 0 AND length(device_label) <= 200),
    created_at    timestamptz NOT NULL,
    last_used_at  timestamptz NOT NULL,
    revoked_at    timestamptz NULL,
    revoke_reason text        NULL
);
CREATE INDEX idx_rtfamilies_person ON identity.refresh_token_families (person_id);
CREATE INDEX idx_rtfamilies_active ON identity.refresh_token_families (person_id, tenant_id) WHERE revoked_at IS NULL;
COMMENT ON TABLE identity.refresh_token_families IS 'Refresh-token family per RFC 9700 §4.13 + ADR 0011. NOT tenant-scoped — session-management infrastructure.';

CREATE TABLE identity.refresh_tokens (
    id              uuid        PRIMARY KEY,
    family_id       uuid        NOT NULL REFERENCES identity.refresh_token_families(id) ON DELETE CASCADE,
    token_hash      text        NOT NULL UNIQUE,
    generation      integer     NOT NULL CHECK (generation >= 0),
    issued_at       timestamptz NOT NULL,
    expires_at      timestamptz NOT NULL,
    consumed_at     timestamptz NULL,
    replaced_by_id  uuid        NULL REFERENCES identity.refresh_tokens(id)
);
CREATE INDEX idx_rtokens_family_gen ON identity.refresh_tokens (family_id, generation);
COMMENT ON TABLE identity.refresh_tokens IS 'Individual tokens within a family. Hash-only storage; plaintext NEVER persisted. RFC 9700 reuse detection in domain layer.';

-- ============================================================================
-- identity.auth_routing
-- Non-RLS index for cross-tenant resolution: email → (person_id, tenant_id).
-- Maintained by Watermill subscribers reacting to identity events
-- (PersonCreated, MembershipActivated, MembershipDeactivated, etc.).
-- ============================================================================

CREATE TABLE identity.auth_routing (
    email             text        PRIMARY KEY CHECK (length(email) <= 254),
    person_id         uuid        NOT NULL,
    active_tenant_id  uuid        NULL,           -- null when no active membership
    updated_at        timestamptz NOT NULL
);
COMMENT ON TABLE identity.auth_routing IS 'Cross-tenant email→tenant index for login. NOT RLS-scoped (intentional — login flow predates tenant context). Maintained via Watermill events.';

-- ============================================================================
-- identity.outbox
-- Per-module outbox table per ADR 0008 + 0027. Doubles as audit log.
-- Tenant-scoped + RLS so per-tenant audit queries are clean.
-- ============================================================================

CREATE TABLE identity.outbox (
    id            uuid        PRIMARY KEY,
    tenant_id     uuid        NOT NULL,           -- denormalised; events without tenant scope use a sentinel
    topic         text        NOT NULL,
    payload       jsonb       NOT NULL,
    occurred_at   timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    forwarded     boolean     NOT NULL DEFAULT false,
    forwarded_at  timestamptz NULL
);
CREATE INDEX idx_outbox_unforwarded ON identity.outbox (created_at) WHERE NOT forwarded;
CREATE INDEX idx_outbox_tenant_topic_occurred ON identity.outbox (tenant_id, topic, occurred_at DESC);

ALTER TABLE identity.outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.outbox FORCE  ROW LEVEL SECURITY;

CREATE POLICY outbox_select ON identity.outbox
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY outbox_insert ON identity.outbox
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

-- The forwarder process needs UPDATE to flip `forwarded = true` AFTER publish.
-- It runs in platform-operator mode so the policy permits cross-tenant updates.
CREATE POLICY outbox_modify ON identity.outbox
    FOR UPDATE
    USING (app.is_platform())
    WITH CHECK (app.is_platform());

COMMENT ON TABLE identity.outbox IS 'Domain events written same-tx as aggregate state. Watermill SQL forwarder polls + republishes. Doubles as audit log per ADR 0027.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS identity.outbox CASCADE;
DROP TABLE IF EXISTS identity.auth_routing CASCADE;
DROP TABLE IF EXISTS identity.refresh_tokens CASCADE;
DROP TABLE IF EXISTS identity.refresh_token_families CASCADE;
DROP TABLE IF EXISTS identity.tenant_memberships CASCADE;
DROP TABLE IF EXISTS identity.persons CASCADE;
DROP TABLE IF EXISTS identity.tenants CASCADE;
DROP SCHEMA IF EXISTS identity CASCADE;

-- +goose StatementEnd
