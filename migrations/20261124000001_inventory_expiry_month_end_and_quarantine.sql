-- LeadKart Go — Inventory G2: month-end expiry and the quarantine bucket.
--
-- A medicine label prints a month; the law reads it as valid through the last
-- day of that month (Drugs Rules, Rule 96). A batch therefore stores the LAST
-- DAY of the printed month, and the database refuses any other date, so every
-- stock query can compare expiry_date to one bound (common/expiry).
--
-- Expired stock may be kept only apart and marked "Not for sale" (Rule 65(17)):
-- units are moved into quantity_quarantined, which is part of quantity_on_hand
-- (they are still in the building) and is never available to sell. Quarantine,
-- release and write-off are STATE CHANGES recorded as ledger rows; nothing is
-- deleted (the ledger is append-only and has no delete policy).
--
-- Recorded in ADR 0070 (the held-units constraint supersedes the reserved-only
-- one of ADR 0063 section 4).
--
-- No row is rewritten: nothing is live, and a database holding a batch whose
-- expiry is not a month end must be emptied or corrected by hand.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE inventory.batches
    ADD CONSTRAINT chk_batches_expiry_is_month_end
    CHECK (expiry_date = (date_trunc('month', expiry_date::timestamp) + interval '1 month' - interval '1 day')::date);

ALTER TABLE inventory.batches
    ADD COLUMN quantity_quarantined bigint NOT NULL DEFAULT 0 CHECK (quantity_quarantined >= 0);

-- Held units (reserved for orders, set apart in quarantine) never exceed what is
-- on hand; this replaces the reserved-only constraint.
ALTER TABLE inventory.batches DROP CONSTRAINT chk_batches_reserved_le_onhand;
ALTER TABLE inventory.batches
    ADD CONSTRAINT chk_batches_held_le_onhand
    CHECK (quantity_reserved + quantity_quarantined <= quantity_on_hand);

COMMENT ON COLUMN inventory.batches.quantity_quarantined IS
    'Units set apart as "Not for sale" (expired or otherwise held). Part of quantity_on_hand; never reservable. Moved by quarantine / release_quarantine / write_off ledger rows.';

ALTER TABLE inventory.stock_movements DROP CONSTRAINT stock_movements_type_check;
ALTER TABLE inventory.stock_movements
    ADD CONSTRAINT stock_movements_type_check
    CHECK (type IN ('inbound','outbound','adjustment','reservation','release',
                    'quarantine','release_quarantine','write_off'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE inventory.stock_movements DROP CONSTRAINT stock_movements_type_check;
ALTER TABLE inventory.stock_movements
    ADD CONSTRAINT stock_movements_type_check
    CHECK (type IN ('inbound','outbound','adjustment','reservation','release'));

ALTER TABLE inventory.batches DROP CONSTRAINT chk_batches_held_le_onhand;
ALTER TABLE inventory.batches
    ADD CONSTRAINT chk_batches_reserved_le_onhand CHECK (quantity_reserved <= quantity_on_hand);
ALTER TABLE inventory.batches DROP COLUMN quantity_quarantined;
ALTER TABLE inventory.batches DROP CONSTRAINT chk_batches_expiry_is_month_end;

-- +goose StatementEnd
