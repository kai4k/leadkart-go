-- LeadKart Go — Partial-unique-index discipline sweep for soft-deletable
-- tables.
--
-- Brandur "Postgres unique indexes for distributed locks" canon +
-- ADR 0027 discipline: on a table that supports soft-delete, every
-- UNIQUE INDEX whose columns overlap the deletable surface must carry
-- a `WHERE NOT is_deleted` (or `WHERE deleted_at IS NULL`) predicate.
-- Without the predicate, restoring (or recreating) a row collides
-- with the still-indexed soft-deleted ghost.
--
-- This migration is the canonical "fix any violations" sweep for the
-- arch-test `TestArch_PartialUniqueIndexWithSoftDelete`. As of the
-- 2026-06-03 audit pass the predicate finds ZERO violations across
-- the migrations/ tree — every UNIQUE INDEX on a soft-deletable
-- table already carries the partial-WHERE clause:
--
--   identity.roles
--     uq_roles_tenant_name  → WHERE NOT is_deleted (file 0002)
--   inventory.products
--     uq_products_tenant_sku_live      → WHERE NOT is_deleted (file 0001)
--   inventory.batches
--     uq_batches_product_number_live   → WHERE NOT is_deleted (file 0001)
--
-- The migration is intentionally a documented NO-OP: it codifies the
-- audit verdict + ships in the same numbering block as the audit-chain
-- backfill (20260603000301) so the discipline-sweep wave is grep-able
-- by date prefix. If a future drift introduces a violating index, the
-- arch test now (post this wave) fails hard with `t.Errorf` — the
-- offending PR ships its own replacement here.
--
-- Pattern for future fixes (one block per violating index):
--
--   DROP INDEX IF EXISTS <schema>.<old_index_name>;
--   CREATE UNIQUE INDEX <new_index_name>
--       ON <schema>.<table> (<cols>)
--       WHERE NOT is_deleted;

-- +goose Up
-- +goose StatementBegin

-- (no-op — see header comment for audit verdict)
SELECT 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- (no-op)
SELECT 1;

-- +goose StatementEnd
