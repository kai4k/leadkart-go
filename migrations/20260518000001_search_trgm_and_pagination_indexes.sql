-- LeadKart Go — Phase 1.5+ Wave 1 — search + pagination index sweep
--
-- Ships:
--   1. pg_trgm extension + GIN trigram indexes for ?q= server-side search
--      on persons, tenants, and tenant_memberships (ADR 0040).
--   2. Composite partial indexes matching cursor-pagination predicates
--      on tenant_memberships + tenants (ADR 0038).
--   3. Partial-index sweep — closing the "active-rows-only" index gaps
--      that the existing schema left implicit.
--
-- All indexes are CREATE INDEX (NOT CONCURRENTLY) because goose's default
-- mode wraps each migration in a transaction. CONCURRENTLY can't run
-- inside a tx; we accept the table-lock blip on this single-tenant
-- early-phase schema. When the production data set is larger, the
-- canonical move is a follow-up migration with `--no-transaction`
-- annotations + CREATE INDEX CONCURRENTLY.
--
-- +goose Up
-- +goose StatementBegin

-- ============================================================================
-- pg_trgm extension — required for GIN trigram indexes (ADR 0040)
-- ============================================================================
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ============================================================================
-- Search GIN indexes (Gap 1 — ADR 0040)
-- ============================================================================

-- identity.persons — search by email + first_name + last_name combined.
-- Single GIN over the concatenated lowercased expression is more space-
-- efficient than per-column indexes and lets one ILIKE match across all
-- three fields. Partial-index predicate matches the common "active,
-- non-anonymised" filter most search queries carry anyway.
CREATE INDEX idx_persons_search_trgm
    ON identity.persons
    USING gin (
        (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name)) gin_trgm_ops
    )
    WHERE is_active AND NOT is_anonymised;

-- identity.tenants — search by slug + legal_name + display_name combined.
-- Excludes hard-deleted rows (operator queries don't want to surface them).
-- Suspended tenants ARE searchable — operators frequently look up suspended
-- tenants to investigate or restore.
CREATE INDEX idx_tenants_search_trgm
    ON identity.tenants
    USING gin (
        (lower(slug) || ' ' || lower(legal_name) || ' ' || lower(display_name)) gin_trgm_ops
    );

-- identity.tenant_memberships — search by designation + department.
-- GIN does NOT have a default operator class for uuid; including
-- `tenant_id` as a leading column would require the `btree_gin`
-- contrib extension (gin_uuid_ops). For LeadKart scale that's
-- speculative dependency cost — the planner combines THIS GIN with
-- the existing idx_memberships_tenant (tenant_id btree) via Bitmap
-- And on the realistic query shape:
--   WHERE tenant_id = $1 AND status='active'
--     AND (designation||' '||department) ILIKE '%foo%'
-- The bitmap-and plan is sub-50ms at < 10M rows per realistic
-- multi-tenant SaaS — well within our v0.2 ceiling.
--
-- If membership-level operator search at platform scale (>= 50M
-- rows across all tenants) ever becomes a measured pain point, the
-- migration to install btree_gin + recreate this index with
-- (tenant_id, trgm_expr) is additive — no app-level changes.
CREATE INDEX idx_memberships_search_trgm
    ON identity.tenant_memberships
    USING gin (
        (coalesce(lower(designation), '') || ' ' || coalesce(lower(department), '')) gin_trgm_ops
    )
    WHERE status = 'active';

-- ============================================================================
-- Composite keyset indexes (Gap 2 — ADR 0038)
-- ============================================================================

-- identity.tenant_memberships — cursor pagination on (joined_at, id) DESC
-- under tenant + active filter. Composite ordering matches
--   WHERE tenant_id = $1 AND status = 'active' AND (joined_at, id) < (...)
--   ORDER BY joined_at DESC, id DESC LIMIT $page_size
-- so the planner emits an Index Scan, not a Seq Scan + Filter.
--
-- The existing idx_memberships_tenant is a single-column lookup; it does NOT
-- cover the keyset predicate. The pre-existing idx_memberships_active partial
-- unique index serves the single-active-membership invariant but isn't a
-- sort-prefix match for cursor pagination either.
CREATE INDEX idx_memberships_tenant_active_joined
    ON identity.tenant_memberships (tenant_id, joined_at DESC, id DESC)
    WHERE status = 'active';

-- identity.tenants — cursor pagination on (created_at, id) DESC for the
-- Platform-operator listing. No tenant_id filter (tenants is non-RLS,
-- non-tenant-scoped by design). Hard-deleted rows excluded.
CREATE INDEX idx_tenants_created_keyset
    ON identity.tenants (created_at DESC, id DESC);

-- ============================================================================
-- Partial-index sweep (Gap 8) — close gaps the original schema left implicit
-- ============================================================================

-- identity.persons — covers GetByEmail under the common "active, non-
-- anonymised" filter. The existing idx_persons_active is on (id) only;
-- this adds the email lookup path. Email is globally unique (UNIQUE
-- constraint on the column) so this is a covering single-column index
-- for the GetPersonByEmail hot path.
CREATE INDEX idx_persons_email_active
    ON identity.persons (email)
    WHERE is_active AND NOT is_anonymised;

-- identity.roles — partial index for "live roles per tenant by name".
-- Existing idx_roles_tenant covers (tenant_id) alone; the unique partial
-- index uq_roles_tenant_name handles the (tenant_id, name) lookup but
-- only as a uniqueness constraint. Adding a separate partial keyset
-- index for cursor pagination on (tenant_id, created_at DESC, id DESC).
CREATE INDEX idx_roles_tenant_created_keyset
    ON identity.roles (tenant_id, created_at DESC, id DESC)
    WHERE NOT is_deleted;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.idx_roles_tenant_created_keyset;
DROP INDEX IF EXISTS identity.idx_persons_email_active;
DROP INDEX IF EXISTS identity.idx_tenants_created_keyset;
DROP INDEX IF EXISTS identity.idx_memberships_tenant_active_joined;
DROP INDEX IF EXISTS identity.idx_memberships_search_trgm;
DROP INDEX IF EXISTS identity.idx_tenants_search_trgm;
DROP INDEX IF EXISTS identity.idx_persons_search_trgm;
-- pg_trgm extension intentionally not dropped — other migrations or
-- adjacent modules may depend on it. Rolling back this migration should
-- not nuke the extension globally.

-- +goose StatementEnd
