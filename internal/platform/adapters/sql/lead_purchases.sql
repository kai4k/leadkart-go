-- Platform module — marketplace purchase + tier queries. Per ADR 0065.

-- name: InsertLeadPurchase :exec
-- One buyer row. UNIQUE(lead_id, tenant_id) makes a double-buy a 23505 that
-- the adapter translates to ErrAlreadyPurchased.
INSERT INTO platform.lead_purchases (
    id, lead_id, tenant_id, created_by_membership_id, amount_paisa, purchased_at
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: ListLeadPurchaseTenantIDs :many
-- Buyer tenant IDs for a lead — hydrates the aggregate's buyer set so
-- RecordPurchase can enforce no-double-buy + the sale-limit count. Read
-- inside the purchase tx after the lead row is locked (GetPlatformLeadByID
-- ForUpdate) so the count is race-free.
SELECT tenant_id
FROM   platform.lead_purchases
WHERE  lead_id = $1
ORDER  BY purchased_at ASC, id ASC;

-- name: GetLeadTier :one
-- Per-tier config: default sale limit + base price.
SELECT code, default_sale_limit, base_price_paisa
FROM   platform.lead_tiers
WHERE  code = $1;
