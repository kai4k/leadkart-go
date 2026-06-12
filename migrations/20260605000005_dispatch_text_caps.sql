-- LeadKart Go — Dispatch text-column length caps (audit follow-up).
--
-- dispatch_init shipped docket_number + failure_reason as unbounded text;
-- carrier_name already carries a 1..200 CHECK. Cap the remaining free-text
-- columns so an authenticated caller (or misbehaving carrier webhook) cannot
-- bloat storage with multi-MB strings (OWASP API4 belt-and-braces — the HTTP
-- edge also caps request bodies, this is the DB-level guarantee).
--
-- Bounds: docket numbers are carrier tracking codes (longest real-world
-- formats < 50 chars; 100 gives slack). failure_reason is an operator/carrier
-- sentence, 1000 chars is ample.
--
-- +goose Up
-- +goose StatementBegin

ALTER TABLE dispatch.consignment_notes
    ADD CONSTRAINT chk_dispatch_docket_number_len CHECK (length(docket_number) <= 100),
    ADD CONSTRAINT chk_dispatch_failure_reason_len CHECK (length(failure_reason) <= 1000);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE dispatch.consignment_notes
    DROP CONSTRAINT IF EXISTS chk_dispatch_docket_number_len,
    DROP CONSTRAINT IF EXISTS chk_dispatch_failure_reason_len;

-- +goose StatementEnd
