-- LeadKart Go — Dispatch module init (ADR 0063 fulfillment saga, BRD §6.6).
--
-- dispatch.consignment_notes — the transport document (formal "consignment
-- note" / informal "builty") that travels with goods warehouse → consignee.
-- One row per shipment; created by the orders.order_packed.v1 subscriber (a
-- pending slot) and advanced through its status state machine by operator /
-- carrier-webhook driven transitions.
--
-- State machine (CHECK-enforced enum): pending → dispatched → in_transit →
-- delivered (terminal) | failed (terminal).
--
-- Cross-module reference: order_id points at orders.orders(id). The composite
-- FK is DEFERRED until the Orders module ships its schema — modelled here as a
-- plain uuid + a UNIQUE(tenant_id, order_id) natural-key (at most one note per
-- order; underpins the create-handler idempotency). Tenant-scoped, RLS+FORCE.
--
-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS dispatch;

CREATE TABLE dispatch.consignment_notes (
    id                       uuid        PRIMARY KEY,
    tenant_id                uuid        NOT NULL,
    order_id                 uuid        NOT NULL,
    status                   text        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','dispatched','in_transit','delivered','failed')),
    carrier_name             text        NOT NULL CHECK (length(carrier_name) BETWEEN 1 AND 200),
    docket_number            text        NOT NULL DEFAULT '',
    box_count                integer     NOT NULL CHECK (box_count > 0),
    weight_grams             bigint      NOT NULL CHECK (weight_grams > 0),
    expected_delivery_at     timestamptz NULL,
    dispatched_at            timestamptz NULL,
    in_transit_at            timestamptz NULL,
    delivered_at             timestamptz NULL,
    failed_at                timestamptz NULL,
    failure_reason           text        NOT NULL DEFAULT '',
    created_at               timestamptz NOT NULL,
    created_by_membership_id uuid        NOT NULL,

    -- At most one consignment note per order (per tenant). The create
    -- handler relies on this for idempotency under OrderPacked replay.
    UNIQUE (tenant_id, order_id)
);

-- Operator dashboard "my shipments" keyset on (created_at, id) DESC.
CREATE INDEX idx_cn_tenant_created_keyset
    ON dispatch.consignment_notes (tenant_id, created_at DESC, id DESC);

-- Status-filtered board ("show me everything in_transit").
CREATE INDEX idx_cn_tenant_status
    ON dispatch.consignment_notes (tenant_id, status);

ALTER TABLE dispatch.consignment_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE dispatch.consignment_notes FORCE ROW LEVEL SECURITY;

CREATE POLICY cn_select ON dispatch.consignment_notes
    FOR SELECT
    USING (tenant_id = app.current_tenant() OR app.is_platform());

CREATE POLICY cn_write ON dispatch.consignment_notes
    FOR ALL
    USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE dispatch.consignment_notes IS
    'Transport document per shipment (ADR 0063). State machine: pending → dispatched → in_transit → delivered | failed. One per order via UNIQUE(tenant_id, order_id). Tenant-scoped RLS.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS dispatch.consignment_notes CASCADE;
DROP SCHEMA IF EXISTS dispatch CASCADE;

-- +goose StatementEnd
