-- LeadKart Go — Inventory G2: expiry alerts are emitted once per TIER.
--
-- The dedup key of an alert emission was (tenant, kind, subject, day), so a batch
-- inside its warning window raised the same alert every day. It is now
-- (tenant, kind, subject, emission_key): an expiry alert uses "tier:<days>" (one
-- per warning tier crossed), the reorder alert "day:<business day>" (once a day
-- while below the level), and the new expired_on_hand kind "once" per batch.
-- The row stays a ledger: emitted_date and emitted_at record when. Recorded in
-- ADR 0070 (the warning tiers are a company setting).

-- +goose Up
-- +goose StatementBegin

ALTER TABLE inventory.alert_emissions DROP CONSTRAINT alert_emissions_pkey;
ALTER TABLE inventory.alert_emissions DROP CONSTRAINT alert_emissions_kind_check;

ALTER TABLE inventory.alert_emissions ADD COLUMN emission_key text NOT NULL DEFAULT '' CHECK (length(emission_key) <= 40);
ALTER TABLE inventory.alert_emissions ALTER COLUMN emission_key DROP DEFAULT;

ALTER TABLE inventory.alert_emissions
    ADD CONSTRAINT alert_emissions_kind_check
    CHECK (kind IN ('batch_expiring', 'product_below_reorder', 'expired_on_hand'));
ALTER TABLE inventory.alert_emissions
    ADD PRIMARY KEY (tenant_id, kind, subject_id, emission_key);

COMMENT ON TABLE inventory.alert_emissions IS
    'Dedup ledger for the expiry and reorder scans. PK (tenant, kind, subject, emission_key): one expiry alert per warning tier, one reorder alert per business day, one expired-on-hand alert per batch.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE inventory.alert_emissions DROP CONSTRAINT alert_emissions_pkey;
ALTER TABLE inventory.alert_emissions DROP CONSTRAINT alert_emissions_kind_check;
DELETE FROM inventory.alert_emissions WHERE kind = 'expired_on_hand';
-- Several tiers of one batch on one date would collide on the old key: keep the first.
DELETE FROM inventory.alert_emissions a USING inventory.alert_emissions b
 WHERE a.tenant_id = b.tenant_id AND a.kind = b.kind AND a.subject_id = b.subject_id
   AND a.emitted_date = b.emitted_date AND a.ctid > b.ctid;
ALTER TABLE inventory.alert_emissions DROP COLUMN emission_key;
ALTER TABLE inventory.alert_emissions
    ADD CONSTRAINT alert_emissions_kind_check CHECK (kind IN ('batch_expiring', 'product_below_reorder'));
ALTER TABLE inventory.alert_emissions
    ADD PRIMARY KEY (tenant_id, kind, subject_id, emitted_date);

-- +goose StatementEnd
