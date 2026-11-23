-- LeadKart Go — Inventory G2 fix: a real return_to_maker movement type.
--
-- Expired stock may be set apart, returned to the maker or written off (Drugs
-- Rules, Rule 65(17)). A return was recorded as a write-off; it now has its own
-- ledger type, so the ledger says what happened to the units. Rows are only ever
-- added: this changes the allowed set, it deletes nothing (ADR 0071: no data is
-- deleted). Recorded in ADR 0070.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE inventory.stock_movements DROP CONSTRAINT stock_movements_type_check;
ALTER TABLE inventory.stock_movements
    ADD CONSTRAINT stock_movements_type_check
    CHECK (type IN ('inbound','outbound','adjustment','reservation','release',
                    'quarantine','release_quarantine','write_off','return_to_maker'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- NOT VALID: the narrower set binds NEW rows only. A ledger row of the type this
-- migration added already exists once the type has been used, and a Down that
-- validated it would fail on it, or would have to delete or rewrite that row (ADR
-- 0071: no data is deleted). The row stays as it was written.
ALTER TABLE inventory.stock_movements DROP CONSTRAINT stock_movements_type_check;
ALTER TABLE inventory.stock_movements
    ADD CONSTRAINT stock_movements_type_check
    CHECK (type IN ('inbound','outbound','adjustment','reservation','release',
                    'quarantine','release_quarantine','write_off')) NOT VALID;

-- +goose StatementEnd
