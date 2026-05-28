-- LeadKart Go — Recreate every outbox index to include `id` as the
-- ORDER BY tiebreaker per ADR 0027 + the
-- TestArch_OutboxSelectsOrderByMonotonicTiebreaker fitness function.
--
-- THE BUG (migration 20260505000002 + 20260601000001 + 20260602000001
-- + 20260603000001):
--
--   CREATE INDEX idx_outbox_unforwarded
--       ON identity.outbox (created_at) WHERE NOT forwarded;
--   CREATE INDEX idx_outbox_tenant_topic_occurred
--       ON identity.outbox (tenant_id, topic, occurred_at DESC);
--
-- The forwarder query was:
--
--   SELECT ... FROM <schema>.outbox WHERE forwarded = false
--   ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED;
--
-- When two events commit with the SAME `created_at` (a same-tx pair
-- like TenantRegistered → TenantActivated emitted microseconds apart),
-- Postgres returns the tie in UNDEFINED ORDER. The forwarder
-- publishes them to Watermill in undefined order; subscribers see
-- `Activated` before `Registered` → downstream state machines break.
--
-- THE FIX:
--
-- 1. Hand-written outbox.sql files now `ORDER BY created_at, id`
--    (resp. `ORDER BY occurred_at DESC, id DESC` for the audit
--    history query). UUIDv7 ids are time-monotonic + provide the
--    canonical tiebreaker per Brandur Leach "Transactionally Staged
--    Job Drains in Postgres" + Watermill SQL outbox + ADR 0027 +
--    Chris Richardson *Microservices Patterns* ch.3.
--
-- 2. The matching index must extend its sort key tuple — otherwise
--    Postgres's planner falls back to bitmap scan + in-memory sort,
--    blowing the partial-index seek that made the forwarder polling
--    query fast.
--
-- DROP + CREATE INDEX (not ALTER): Postgres has no `ALTER INDEX`
-- shape that changes the column list. The old indexes are dropped
-- first; the new ones replace them. Per the postgres docs §sql-alterindex
-- — "Only the name of the index, tablespace and storage parameters
-- can be changed via ALTER. To change the column list, drop and
-- recreate."
--
-- WHY NOT CREATE INDEX CONCURRENTLY: goose runs migrations inside a
-- transaction; CREATE INDEX CONCURRENTLY cannot run inside a tx.
-- The outbox tables are small + the index rebuild is fast at our
-- scale (current rows: <10k per tenant). Production-scale tenants
-- (>100k unforwarded rows) would warrant a dedicated CONCURRENT path
-- via a manual maintenance window.
--
-- Surfaced by TestTenantRepository_UpdateByID_PersistsActivatedOutboxEventInSameTx
-- flaking on the feature/money-formatting CI run after Wave 1 merges
-- (PR #46 timeframe). The flake exposed a real production-readable
-- consumer-side ordering bug, not just a test issue.

-- +goose Up
-- +goose StatementBegin

-- identity.outbox -----------------------------------------------------------

DROP INDEX IF EXISTS identity.idx_outbox_unforwarded;
CREATE INDEX idx_outbox_unforwarded
    ON identity.outbox (created_at, id) WHERE NOT forwarded;

DROP INDEX IF EXISTS identity.idx_outbox_tenant_topic_occurred;
CREATE INDEX idx_outbox_tenant_topic_occurred
    ON identity.outbox (tenant_id, topic, occurred_at DESC, id DESC);

-- platform.outbox -----------------------------------------------------------

DROP INDEX IF EXISTS platform.idx_platform_outbox_unforwarded;
CREATE INDEX idx_platform_outbox_unforwarded
    ON platform.outbox (created_at, id) WHERE NOT forwarded;

DROP INDEX IF EXISTS platform.idx_platform_outbox_tenant_topic_occurred;
CREATE INDEX idx_platform_outbox_tenant_topic_occurred
    ON platform.outbox (tenant_id, topic, occurred_at DESC, id DESC);

-- crm.outbox ----------------------------------------------------------------

DROP INDEX IF EXISTS crm.idx_crm_outbox_unforwarded;
CREATE INDEX idx_crm_outbox_unforwarded
    ON crm.outbox (created_at, id) WHERE NOT forwarded;

DROP INDEX IF EXISTS crm.idx_crm_outbox_tenant_topic_occurred;
CREATE INDEX idx_crm_outbox_tenant_topic_occurred
    ON crm.outbox (tenant_id, topic, occurred_at DESC, id DESC);

-- inventory.outbox ----------------------------------------------------------

DROP INDEX IF EXISTS inventory.idx_inventory_outbox_unforwarded;
CREATE INDEX idx_inventory_outbox_unforwarded
    ON inventory.outbox (created_at, id) WHERE NOT forwarded;

DROP INDEX IF EXISTS inventory.idx_inventory_outbox_tenant_topic_occurred;
CREATE INDEX idx_inventory_outbox_tenant_topic_occurred
    ON inventory.outbox (tenant_id, topic, occurred_at DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the original (buggy) indexes. The DOWN migration exists for
-- rollback-test parity per `task migrate:redo`; in production a real
-- rollback would also require reverting the SQL files' ORDER BY +
-- regenerating sqlc.

DROP INDEX IF EXISTS identity.idx_outbox_unforwarded;
CREATE INDEX idx_outbox_unforwarded
    ON identity.outbox (created_at) WHERE NOT forwarded;

DROP INDEX IF EXISTS identity.idx_outbox_tenant_topic_occurred;
CREATE INDEX idx_outbox_tenant_topic_occurred
    ON identity.outbox (tenant_id, topic, occurred_at DESC);

DROP INDEX IF EXISTS platform.idx_platform_outbox_unforwarded;
CREATE INDEX idx_platform_outbox_unforwarded
    ON platform.outbox (created_at) WHERE NOT forwarded;

DROP INDEX IF EXISTS platform.idx_platform_outbox_tenant_topic_occurred;
CREATE INDEX idx_platform_outbox_tenant_topic_occurred
    ON platform.outbox (tenant_id, topic, occurred_at DESC);

DROP INDEX IF EXISTS crm.idx_crm_outbox_unforwarded;
CREATE INDEX idx_crm_outbox_unforwarded
    ON crm.outbox (created_at) WHERE NOT forwarded;

DROP INDEX IF EXISTS crm.idx_crm_outbox_tenant_topic_occurred;
CREATE INDEX idx_crm_outbox_tenant_topic_occurred
    ON crm.outbox (tenant_id, topic, occurred_at DESC);

DROP INDEX IF EXISTS inventory.idx_inventory_outbox_unforwarded;
CREATE INDEX idx_inventory_outbox_unforwarded
    ON inventory.outbox (created_at) WHERE NOT forwarded;

DROP INDEX IF EXISTS inventory.idx_inventory_outbox_tenant_topic_occurred;
CREATE INDEX idx_inventory_outbox_tenant_topic_occurred
    ON inventory.outbox (tenant_id, topic, occurred_at DESC);

-- +goose StatementEnd
