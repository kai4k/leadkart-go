-- LeadKart Go — Notifications module initial schema (BRD §6.9).
-- Per ADR 0001 (modular monolith) + ADR 0006 (RLS) + ADR 0038 (cursor
-- pagination) + ADR 0063 §4 (subscriber-decides fan-out).
--
-- Module: Notifications. Owns: Notification (cross-module user-facing alerts).
-- Tenant-scoped — RLS + FORCE. Recipient isolation (you see only YOUR inbox) is
-- applied in the query by recipient_membership_id from the JWT; RLS is the
-- tenant-plane backstop.
--
-- Subscriber-decides (Udi Dahan): this module subscribes to OTHER modules'
-- integration events and decides recipient + content. Producers don't know
-- about notifications. At-least-once delivery + a time-windowed dedup probe
-- make replay safe.

-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS notifications;
COMMENT ON SCHEMA notifications IS 'LeadKart Notifications module — cross-module user-facing alerts (BRD §6.9).';

CREATE TABLE notifications.notifications (
    id                       uuid        PRIMARY KEY,
    tenant_id                uuid        NOT NULL REFERENCES identity.tenants(id),
    recipient_membership_id  uuid        NOT NULL,
    category                 text        NOT NULL CHECK (category IN (
                                 'lead_assigned','lead_converted','order_confirmed',
                                 'order_delivered','work_item_assigned','work_item_overdue',
                                 'stock_expiring','stock_below_reorder','reminder')),
    title                    text        NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    body                     text        NOT NULL DEFAULT '' CHECK (length(body) <= 2000),
    state                    text        NOT NULL DEFAULT 'unread'
                                             CHECK (state IN ('unread','read','dismissed')),
    -- Source-entity tuple: all-set (sourced) or all-empty (manual). Drives the
    -- dedup probe + the click-through deep-link.
    source_module            text        NOT NULL DEFAULT '' CHECK (length(source_module) <= 50),
    source_entity_type       text        NOT NULL DEFAULT '' CHECK (length(source_entity_type) <= 100),
    source_entity_id         text        NOT NULL DEFAULT '' CHECK (length(source_entity_id) <= 100),
    deep_link                text        NOT NULL DEFAULT '' CHECK (length(deep_link) <= 500),
    -- The membership whose action triggered this notification (audit chain).
    -- NULL for system/cron-sourced notifications (e.g. overdue sweeps).
    created_by_membership_id uuid        NULL,
    created_at               timestamptz NOT NULL,
    read_at                  timestamptz NULL,
    dismissed_at             timestamptz NULL
);

-- Inbox keyset pagination: newest-first per recipient (ADR 0038).
CREATE INDEX idx_notifications_recipient_keyset
    ON notifications.notifications (tenant_id, recipient_membership_id, created_at DESC, id DESC);

-- Unread badge count: partial index on the hot unread slice.
CREATE INDEX idx_notifications_recipient_unread
    ON notifications.notifications (tenant_id, recipient_membership_id)
    WHERE state = 'unread';

-- Dedup probe support: the Add path checks for a same-source notification to the
-- same recipient within a short window before inserting (at-least-once replay
-- safety). Partial — only sourced notifications dedup; manual ones never do.
CREATE INDEX idx_notifications_dedup
    ON notifications.notifications
        (tenant_id, recipient_membership_id, source_module, source_entity_type, source_entity_id, category, created_at DESC)
    WHERE source_entity_id <> '';

ALTER TABLE notifications.notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications.notifications FORCE  ROW LEVEL SECURITY;

CREATE POLICY notifications_select ON notifications.notifications
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY notifications_insert ON notifications.notifications
    FOR INSERT
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY notifications_modify ON notifications.notifications
    FOR UPDATE
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY notifications_delete ON notifications.notifications
    FOR DELETE
    USING (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE notifications.notifications IS
    'Notification aggregate. Tenant-scoped, FORCE RLS. Recipient-scoped at query time. unread → read/dismissed lifecycle; purge cron (read 7d, unread 30d).';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS notifications.notifications CASCADE;
DROP SCHEMA IF EXISTS notifications CASCADE;

-- +goose StatementEnd
