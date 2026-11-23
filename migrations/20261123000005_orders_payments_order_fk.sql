-- LeadKart Go — a payment must belong to an order that exists for the tenant.
--
-- orders.payments.order_id had no foreign key; only the record-payment handler
-- refused an unknown order. The database now refuses it too, on the tenant-scoped
-- composite key, so a second writer or a future code path cannot leave an orphan.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE orders.payments
    ADD CONSTRAINT fk_orders_payments_order
    FOREIGN KEY (tenant_id, order_id) REFERENCES orders.orders (tenant_id, id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE orders.payments DROP CONSTRAINT fk_orders_payments_order;

-- +goose StatementEnd
