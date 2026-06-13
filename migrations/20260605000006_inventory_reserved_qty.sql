-- LeadKart Go — Inventory true-hold reservation (ADR 0063 §4).
--
-- Adds quantity_reserved to inventory.batches so the platform can SOFT-COMMIT
-- available stock to a confirmed order and prevent oversell: two orders can no
-- longer both reserve the same physical units. This SUPERSEDES the earlier
-- audit-only stance (stock_movements reservation/release rows were recorded but
-- non-mutating; the "Orders module tracks reserved qty separately" plan never
-- materialised). Reserved quantity now lives with the stock it holds — the
-- Batch aggregate — which is the correct DDD altitude (Vernon IDDD ch.10).
--
-- Lifecycle (driven by inventory subscribers on the Orders events):
--   orders.order_confirmed.v1 → reserve (FEFO; quantity_reserved += qty)
--   orders.order_packed.v1    → consume (Release + Outbound: reserved−q, on-hand−q)
--   orders.order_cancelled.v1 → release  (quantity_reserved −= qty) if still held
--
-- Invariant enforced at the DB (defence-in-depth alongside the domain guard):
--   0 <= quantity_reserved <= quantity_on_hand  (available is never negative).

-- +goose Up
-- +goose StatementBegin

ALTER TABLE inventory.batches
    ADD COLUMN quantity_reserved bigint NOT NULL DEFAULT 0
        CHECK (quantity_reserved >= 0);

-- Available (on-hand − reserved) is never negative: reserved is capped at
-- on-hand. Outbound/Adjustment-down respect this too (cannot ship reserved
-- units), so the constraint holds across every movement type.
ALTER TABLE inventory.batches
    ADD CONSTRAINT chk_batches_reserved_le_onhand
        CHECK (quantity_reserved <= quantity_on_hand);

COMMENT ON COLUMN inventory.batches.quantity_reserved IS
    'Units soft-committed to confirmed orders but not yet shipped (ADR 0063 §4 true-hold). available = quantity_on_hand − quantity_reserved.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE inventory.batches
    DROP CONSTRAINT IF EXISTS chk_batches_reserved_le_onhand;
ALTER TABLE inventory.batches
    DROP COLUMN IF EXISTS quantity_reserved;

-- +goose StatementEnd
