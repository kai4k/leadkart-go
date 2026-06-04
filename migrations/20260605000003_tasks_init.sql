-- LeadKart Go — Phase C.2 — Tasks module init (BRD §6.8 + ADR 0001).
--
-- Ships the Tasks bounded context:
--
--   tasks.work_items  → tenant-scoped task aggregate (state machine +
--                       auto-creation idempotency + hierarchy-gated
--                       assignment)
--
-- No per-module outbox: integration events ride the shared common.outbox
-- relay (ADR 0064/0067), not a tasks.outbox table.
--
-- All tables RLS+FORCE per multi-tenancy.md "FORCE ROW LEVEL SECURITY".
-- Every Tasks table is tenant-scoped — work items NEVER cross tenant.
--
-- BRD §6.8 surface:
--   types       → manual | callback_reminder | reorder_reminder | follow_up | custom
--   priorities  → low | medium | high | urgent
--   states      → pending | in_progress | completed | overdue | cancelled
--
-- Auto-creation idempotency: a single (source_entity_type,
-- source_entity_id) MUST yield at most one OPEN work item per tenant
-- (pending OR in_progress). Replays of the same source event collapse
-- via the partial unique index uq_tasks_source_open.
--
-- Bulk assignment: batch_id NULL for individual creation; populated for
-- "assign 50 leads to team X" workflows.
--
-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS tasks;
COMMENT ON SCHEMA tasks IS 'LeadKart Tasks module — WorkItem aggregate per BRD §6.8.';

-- ============================================================================
-- tasks.work_items
-- ============================================================================

CREATE TABLE tasks.work_items (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    type                        text        NOT NULL CHECK (type IN ('manual','callback_reminder','reorder_reminder','follow_up','custom')),
    priority                    text        NOT NULL CHECK (priority IN ('low','medium','high','urgent')),
    state                       text        NOT NULL CHECK (state IN ('pending','in_progress','completed','overdue','cancelled')),
    title                       text        NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    description                 text        NOT NULL DEFAULT '',

    assigned_to_membership_id   uuid        NOT NULL,
    assigned_by_membership_id   uuid        NOT NULL,

    due_at                      timestamptz NOT NULL,
    completed_at                timestamptz NULL,
    cancelled_at                timestamptz NULL,
    cancellation_reason         text        NOT NULL DEFAULT '',

    batch_id                    uuid        NULL,
    source_module               text        NOT NULL DEFAULT '',
    source_entity_type          text        NULL,
    source_entity_id            text        NULL,

    created_at                  timestamptz NOT NULL,
    created_by_membership_id    uuid        NOT NULL,

    is_deleted                  boolean     NOT NULL DEFAULT false,
    deleted_at                  timestamptz NULL,

    PRIMARY KEY (tenant_id, id),

    -- Source-tuple consistency: either ALL source_* fields are unset
    -- (manual / ad-hoc) OR all three are set (subscriber-created from a
    -- cross-module event).
    CONSTRAINT chk_source_consistency CHECK (
        (source_module = '' AND source_entity_type IS NULL AND source_entity_id IS NULL)
        OR
        (source_module <> '' AND source_entity_type IS NOT NULL AND source_entity_id IS NOT NULL)
    )
);

-- Idempotency for subscriber-created tasks: at most ONE open work_item
-- per (source_entity_type, source_entity_id) per tenant. Re-deliveries
-- of the same event collapse. Soft-deleted + terminal tasks are excluded
-- so legitimately new tasks for the same source can be raised later.
CREATE UNIQUE INDEX uq_tasks_source_open
    ON tasks.work_items (tenant_id, source_entity_type, source_entity_id)
    WHERE NOT is_deleted
      AND source_entity_type IS NOT NULL
      AND source_entity_id   IS NOT NULL
      AND state IN ('pending', 'in_progress');

-- "My open tasks" — dashboard / Today / Upcoming / Overdue scans.
CREATE INDEX idx_tasks_assignee_due
    ON tasks.work_items (tenant_id, assigned_to_membership_id, due_at)
    WHERE NOT is_deleted AND state IN ('pending', 'in_progress', 'overdue');

-- Overdue scanner — periodic job lists candidates whose due_at < now()
-- in pending/in_progress. Partial index keeps the scan cheap.
CREATE INDEX idx_tasks_overdue_scan
    ON tasks.work_items (tenant_id, due_at)
    WHERE NOT is_deleted AND state IN ('pending', 'in_progress');

-- Bulk-batch lookups (admin "show me everything in this batch").
CREATE INDEX idx_tasks_batch
    ON tasks.work_items (tenant_id, batch_id)
    WHERE batch_id IS NOT NULL AND NOT is_deleted;

-- Cursor-paginated list (default sort) — keyset on (due_at, id) DESC.
CREATE INDEX idx_tasks_tenant_due_id
    ON tasks.work_items (tenant_id, due_at DESC, id DESC)
    WHERE NOT is_deleted;

-- Purge scan — completed/cancelled tasks older than retention.
CREATE INDEX idx_tasks_purge_scan
    ON tasks.work_items (tenant_id, completed_at, cancelled_at)
    WHERE NOT is_deleted AND state IN ('completed', 'cancelled');

ALTER TABLE tasks.work_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks.work_items FORCE  ROW LEVEL SECURITY;

CREATE POLICY tasks_work_items_select ON tasks.work_items
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tasks_work_items_insert ON tasks.work_items
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tasks_work_items_update ON tasks.work_items
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY tasks_work_items_delete ON tasks.work_items
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE tasks.work_items IS
    'WorkItem aggregate per BRD §6.8. State machine: pending → in_progress → completed | overdue | cancelled. Auto-creation idempotency via (source_entity_type, source_entity_id) partial unique index. Bulk assignment via batch_id.';

-- NOTE: no per-module tasks.outbox table. ADR 0064/0067 mandate ONE
-- shared common.outbox relay (migration 20260604000002) drained by the
-- Watermill library Forwarder; Tasks writes ride it via
-- messaging.PublishOutbox. Schema/table grants are provisioned
-- post-migration (pgtest fixture / production), never self-granted here.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS tasks.work_items CASCADE;
DROP SCHEMA IF EXISTS tasks CASCADE;

-- +goose StatementEnd
