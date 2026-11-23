-- LeadKart Go — FORCE row level security on the tenant tables that only
-- ENABLEd it (crm, tasks, orders, notifications).
--
-- Postgres exempts a table's owner from RLS unless FORCE is set. These
-- tables were created with ENABLE alone, so any connection running as the
-- owner read and wrote every tenant's rows. Every policy already carries the
-- app.is_platform() clause, so FORCE changes nothing for the tenant and
-- platform scopes; it closes the owner bypass. The architecture gate
-- (TestArch_EveryTenantTableHasRLSAndForce) now covers these schemas too.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE crm.crm_leads                    FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.call_logs                    FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.assignment_history           FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.reminders                    FORCE ROW LEVEL SECURITY;

ALTER TABLE tasks.work_items                 FORCE ROW LEVEL SECURITY;

ALTER TABLE orders.quotations                FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.orders                    FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.invoices                  FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.credit_notes              FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.payments                  FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.invoice_number_sequences  FORCE ROW LEVEL SECURITY;

ALTER TABLE notifications.notifications      FORCE ROW LEVEL SECURITY;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE crm.crm_leads                    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.call_logs                    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.assignment_history           NO FORCE ROW LEVEL SECURITY;
ALTER TABLE crm.reminders                    NO FORCE ROW LEVEL SECURITY;

ALTER TABLE tasks.work_items                 NO FORCE ROW LEVEL SECURITY;

ALTER TABLE orders.quotations                NO FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.orders                    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.invoices                  NO FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.credit_notes              NO FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.payments                  NO FORCE ROW LEVEL SECURITY;
ALTER TABLE orders.invoice_number_sequences  NO FORCE ROW LEVEL SECURITY;

ALTER TABLE notifications.notifications      NO FORCE ROW LEVEL SECURITY;

-- +goose StatementEnd
