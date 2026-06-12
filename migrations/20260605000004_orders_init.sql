-- LeadKart Go — Orders module init (BRD §6.4 + ADR 0063 fulfillment saga).
--
-- Ships the Orders bounded context aggregates:
--
--   orders.quotations               → pre-order negotiation (revisions jsonb)
--   orders.orders                   → fulfillment lifecycle state machine
--   orders.invoices                 → append-only tax invoice (one per order)
--   orders.credit_notes             → append-only reversals (credit / cancellation)
--   orders.payments                 → append-only receipts ledger
--   orders.invoice_number_sequences → gapless per-(tenant, FY, kind) counters
--
-- State-based persistence (no event sourcing): aggregates serialise their
-- nested value objects (line items, revisions, tax lines) as JSONB; money is
-- int64 paise (ADR 0061) stored as bigint. Cross-module references
-- (customer_lead_id → crm, consignment_note_id → dispatch) are wire-stable
-- UUID columns, NOT FKs — modular-monolith schema isolation (ADR 0001).
--
-- Gapless numbering (BRD §A-014, ADR 0063 §3): the allocator UPDATEs
-- invoice_number_sequences.last_used inside the order's UoW tx; a rollback
-- rolls back the increment, so numbers never gap.
--
-- All tenant tables RLS+FORCE per multi-tenancy.md. Table/schema grants are
-- provisioned post-migration (pgtest fixture / production), never self-granted.
--
-- +goose Up
-- +goose StatementBegin

CREATE SCHEMA IF NOT EXISTS orders;
COMMENT ON SCHEMA orders IS 'LeadKart Orders module — Quotation / Order / Invoice / CreditNote / Payment aggregates per BRD §6.4 (ADR 0063).';

-- ============================================================================
-- orders.quotations — pre-order negotiation aggregate
-- ============================================================================

CREATE TABLE orders.quotations (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    customer_lead_id            uuid        NOT NULL,
    state                       text        NOT NULL CHECK (state IN ('draft','approved','rejected')),

    -- Full revision history (1-indexed) as JSONB: each revision carries its
    -- line items, note, and reviser. The current revision is the last entry.
    revisions                   jsonb       NOT NULL DEFAULT '[]'::jsonb,

    approved_at                 timestamptz NULL,
    approved_by_membership_id   uuid        NULL,
    rejected_at                 timestamptz NULL,
    rejected_by_membership_id   uuid        NULL,
    rejection_reason            text        NOT NULL DEFAULT '',

    created_at                  timestamptz NOT NULL,
    created_by_membership_id    uuid        NOT NULL,

    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX idx_orders_quotations_tenant_lead_created
    ON orders.quotations (tenant_id, customer_lead_id, created_at DESC, id DESC);
CREATE INDEX idx_orders_quotations_tenant_state_created
    ON orders.quotations (tenant_id, state, created_at DESC, id DESC);

ALTER TABLE orders.quotations ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.quotations FORCE  ROW LEVEL SECURITY;

CREATE POLICY orders_quotations_select ON orders.quotations
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_quotations_insert ON orders.quotations
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_quotations_update ON orders.quotations
    FOR UPDATE USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.quotations IS
    'Quotation aggregate per BRD §6.4. Revisions stored as JSONB; state machine draft → approved | rejected, superseded on revise-after-approve.';

-- ============================================================================
-- orders.orders — fulfillment lifecycle aggregate
-- ============================================================================

CREATE TABLE orders.orders (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    approved_quotation_id       uuid        NOT NULL,
    customer_lead_id            uuid        NOT NULL,
    state                       text        NOT NULL CHECK (state IN (
                                    'quotation_draft','quotation_approved','token_paid','confirmed',
                                    'packed','invoiced','dispatched','delivered','complete','cancelled')),

    -- Snapshot of the confirmed line items (frozen at Order creation from the
    -- approved Quotation revision) + the derived money totals, all in paise.
    confirmed_items             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    subtotal_paise              bigint      NOT NULL DEFAULT 0 CHECK (subtotal_paise >= 0),
    tax_paise                   bigint      NOT NULL DEFAULT 0 CHECK (tax_paise >= 0),
    grand_total_paise           bigint      NOT NULL DEFAULT 0 CHECK (grand_total_paise >= 0),

    -- Cross-module references (wire-stable UUIDs, NOT FKs — ADR 0001).
    invoice_id                  uuid        NULL,
    consignment_note_id         uuid        NULL,

    confirmed_at                timestamptz NULL,
    packed_at                   timestamptz NULL,
    invoiced_at                 timestamptz NULL,
    dispatched_at               timestamptz NULL,
    delivered_at                timestamptz NULL,
    completed_at                timestamptz NULL,
    cancelled_at                timestamptz NULL,
    cancellation_reason         text        NOT NULL DEFAULT '' CHECK (length(cancellation_reason) <= 1000),

    created_at                  timestamptz NOT NULL,
    created_by_membership_id    uuid        NOT NULL,

    PRIMARY KEY (tenant_id, id)
);

-- One Order per approved Quotation (BRD §6.4: a quotation converts once).
CREATE UNIQUE INDEX uq_orders_approved_quotation
    ON orders.orders (tenant_id, approved_quotation_id);

CREATE INDEX idx_orders_orders_tenant_state_created
    ON orders.orders (tenant_id, state, created_at DESC, id DESC);
CREATE INDEX idx_orders_orders_tenant_lead_created
    ON orders.orders (tenant_id, customer_lead_id, created_at DESC, id DESC);

ALTER TABLE orders.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.orders FORCE  ROW LEVEL SECURITY;

CREATE POLICY orders_orders_select ON orders.orders
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_orders_insert ON orders.orders
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_orders_update ON orders.orders
    FOR UPDATE USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.orders IS
    'Order aggregate per BRD §6.4 + ADR 0063 fulfillment saga. Strict state machine quotation_approved → … → complete | cancelled. Confirmed items + totals snapshotted at creation.';

-- ============================================================================
-- orders.invoices — append-only tax invoice (one per order)
-- ============================================================================

CREATE TABLE orders.invoices (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    order_id                    uuid        NOT NULL,

    -- Allocated gapless number, stored as its three components + display form.
    number_kind                 varchar(20) NOT NULL CHECK (number_kind = 'invoice'),
    number_financial_year       text        NOT NULL CHECK (number_financial_year ~ '^\d{4}-\d{2}$'),
    number_seq                  bigint      NOT NULL CHECK (number_seq > 0),
    number_display              text        NOT NULL,

    line_items                  jsonb       NOT NULL DEFAULT '[]'::jsonb,
    tax_lines                   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    subtotal_paise              bigint      NOT NULL CHECK (subtotal_paise >= 0),
    tax_paise                   bigint      NOT NULL CHECK (tax_paise >= 0),
    grand_total_paise           bigint      NOT NULL CHECK (grand_total_paise >= 0),

    issued_at                   timestamptz NOT NULL,
    issued_by_membership_id     uuid        NOT NULL,

    PRIMARY KEY (tenant_id, id),

    -- Same-module referential integrity (CRM precedent: same-schema children
    -- carry FKs; only CROSS-module references stay bare per ADR 0001).
    -- RESTRICT (default): financial documents block parent deletion.
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders.orders (tenant_id, id)
);

-- One invoice per order (BRD §A-014). Cancellation mints a CreditNote, never
-- a second Invoice.
CREATE UNIQUE INDEX uq_orders_invoices_order
    ON orders.invoices (tenant_id, order_id);
-- Numbers are unique within (tenant, FY, kind).
CREATE UNIQUE INDEX uq_orders_invoices_number
    ON orders.invoices (tenant_id, number_financial_year, number_seq);
CREATE INDEX idx_orders_invoices_tenant_issued
    ON orders.invoices (tenant_id, issued_at DESC, id DESC);

ALTER TABLE orders.invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.invoices FORCE  ROW LEVEL SECURITY;

CREATE POLICY orders_invoices_select ON orders.invoices
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_invoices_insert ON orders.invoices
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.invoices IS
    'Invoice aggregate per BRD §A-014. Append-only — one per order; gapless number allocated via orders.invoice_number_sequences.';

-- ============================================================================
-- orders.credit_notes — append-only reversals (credit / cancellation)
-- ============================================================================

CREATE TABLE orders.credit_notes (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    invoice_id                  uuid        NOT NULL,

    number_kind                 varchar(20) NOT NULL CHECK (number_kind IN ('credit_note','cancellation_note')),
    number_financial_year       text        NOT NULL CHECK (number_financial_year ~ '^\d{4}-\d{2}$'),
    number_seq                  bigint      NOT NULL CHECK (number_seq > 0),
    number_display              text        NOT NULL,

    reason                      text        NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    amount_paise                bigint      NOT NULL CHECK (amount_paise > 0),

    issued_at                   timestamptz NOT NULL,
    issued_by_membership_id     uuid        NOT NULL,

    PRIMARY KEY (tenant_id, id),

    -- Same-module referential integrity: a credit note always reverses a
    -- real invoice in this schema.
    FOREIGN KEY (tenant_id, invoice_id) REFERENCES orders.invoices (tenant_id, id)
);

-- At most one cancellation note per invoice (pre-delivery cancellation is a
-- single event); credit notes (post-delivery returns) may stack.
CREATE UNIQUE INDEX uq_orders_credit_notes_cancellation
    ON orders.credit_notes (tenant_id, invoice_id)
    WHERE number_kind = 'cancellation_note';
CREATE UNIQUE INDEX uq_orders_credit_notes_number
    ON orders.credit_notes (tenant_id, number_kind, number_financial_year, number_seq);
CREATE INDEX idx_orders_credit_notes_tenant_invoice
    ON orders.credit_notes (tenant_id, invoice_id, issued_at);

ALTER TABLE orders.credit_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.credit_notes FORCE  ROW LEVEL SECURITY;

CREATE POLICY orders_credit_notes_select ON orders.credit_notes
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_credit_notes_insert ON orders.credit_notes
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.credit_notes IS
    'CreditNote aggregate per BRD §A-014. Append-only; credit_note (post-delivery) stacks, cancellation_note (pre-delivery) at most one per invoice.';

-- ============================================================================
-- orders.payments — append-only receipts ledger
-- ============================================================================

CREATE TABLE orders.payments (
    id                          uuid        NOT NULL,
    tenant_id                   uuid        NOT NULL,
    order_id                    uuid        NOT NULL,

    kind                        text        NOT NULL CHECK (kind IN ('token','full','refund')),
    method                      text        NOT NULL CHECK (method IN ('upi','neft','rtgs','imps','cheque','cash','card_offline')),
    amount_paise                bigint      NOT NULL CHECK (amount_paise > 0),
    external_reference          text        NOT NULL DEFAULT '' CHECK (length(external_reference) <= 100),
    notes                       text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),

    received_at                 timestamptz NOT NULL,
    recorded_at                 timestamptz NOT NULL,
    recorded_by_membership_id   uuid        NOT NULL,

    PRIMARY KEY (tenant_id, id)
);

-- Idempotency for retried webhook receipts: a non-empty external_reference is
-- unique within the tenant.
CREATE UNIQUE INDEX uq_orders_payments_external_reference
    ON orders.payments (tenant_id, external_reference)
    WHERE external_reference <> '';
CREATE INDEX idx_orders_payments_tenant_order_received
    ON orders.payments (tenant_id, order_id, received_at);

ALTER TABLE orders.payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.payments FORCE  ROW LEVEL SECURITY;

CREATE POLICY orders_payments_select ON orders.payments
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_payments_insert ON orders.payments
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.payments IS
    'Payment aggregate per BRD §6.4. Append-only receipts ledger; external_reference unique per tenant for webhook idempotency.';

-- ============================================================================
-- orders.invoice_number_sequences — gapless per-(tenant, FY, kind) counters
-- ============================================================================

CREATE TABLE orders.invoice_number_sequences (
    tenant_id        uuid   NOT NULL,
    financial_year   text   NOT NULL CHECK (financial_year ~ '^\d{4}-\d{2}$'),
    kind             text   NOT NULL CHECK (kind IN ('invoice','credit_note','cancellation_note')),
    last_used        bigint NOT NULL DEFAULT 0 CHECK (last_used >= 0),

    PRIMARY KEY (tenant_id, financial_year, kind)
);

ALTER TABLE orders.invoice_number_sequences ENABLE ROW LEVEL SECURITY;
ALTER TABLE orders.invoice_number_sequences FORCE  ROW LEVEL SECURITY;

-- Allocator runs INSERT … ON CONFLICT DO NOTHING then UPDATE … RETURNING
-- inside the tenant-scoped UoW tx; both INSERT + UPDATE are tenant-gated.
CREATE POLICY orders_invoice_seq_select ON orders.invoice_number_sequences
    FOR SELECT USING (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_invoice_seq_insert ON orders.invoice_number_sequences
    FOR INSERT WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());
CREATE POLICY orders_invoice_seq_update ON orders.invoice_number_sequences
    FOR UPDATE USING (tenant_id = app.current_tenant() OR app.is_platform())
    WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform());

COMMENT ON TABLE orders.invoice_number_sequences IS
    'Gapless invoice-number counters per (tenant, financial_year, kind) per ADR 0063 §3. Allocator increments last_used inside the order UoW tx; rollback rolls back the increment.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS orders.invoice_number_sequences CASCADE;
DROP TABLE IF EXISTS orders.payments CASCADE;
DROP TABLE IF EXISTS orders.credit_notes CASCADE;
DROP TABLE IF EXISTS orders.invoices CASCADE;
DROP TABLE IF EXISTS orders.orders CASCADE;
DROP TABLE IF EXISTS orders.quotations CASCADE;
DROP SCHEMA IF EXISTS orders CASCADE;

-- +goose StatementEnd
