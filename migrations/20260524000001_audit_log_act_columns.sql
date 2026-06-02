-- LeadKart Go — Wave 4 — audit_log_entry actor-chain columns
--
-- ADR 0045 — Scoped JWT impersonation: when an operator opens an
-- impersonation session and acts on behalf of a tenant, the audit
-- log MUST capture three things:
--
--   1. WHO actually acted (sub claim, the synthetic acting identity)
--   2. WHO authorised the action (act.sub claim, the original operator)
--   3. WHY + UNDER WHICH SESSION the action happened
--
-- This migration adds the three nullable columns to capture (2) and
-- (3). Column (1) is already covered by user_id. Non-impersonation
-- rows leave these columns NULL — no migration of existing data.
--
-- Per RFC 8693 §4.1 `act` claim canonical shape; SOC2 CC4.1 + DPDP
-- §12 actor-chain audit requirement.
--
-- +goose Up
-- +goose StatementBegin

ALTER TABLE common.audit_log_entry
    ADD COLUMN act_operator_id uuid NULL,
    ADD COLUMN act_session_id  uuid NULL,
    ADD COLUMN act_reason      text NULL;

-- Per-actor index for forensic queries — "show me every cross-tenant
-- action operator X performed in the last week". Partial index keeps
-- size proportional to actual impersonation usage (most rows have
-- NULL act_* columns).
CREATE INDEX idx_audit_act_operator_occurred
    ON common.audit_log_entry (act_operator_id, occurred_at_utc DESC)
    WHERE act_operator_id IS NOT NULL;

-- Per-session index for "show me every action inside session X".
-- Same partial-index discipline.
CREATE INDEX idx_audit_act_session_occurred
    ON common.audit_log_entry (act_session_id, occurred_at_utc DESC)
    WHERE act_session_id IS NOT NULL;

COMMENT ON COLUMN common.audit_log_entry.act_operator_id IS
    'RFC 8693 act.sub — the original operator who initiated the impersonation session. NULL for non-impersonation rows.';
COMMENT ON COLUMN common.audit_log_entry.act_session_id IS
    'Impersonation session ID — joins to the impersonation store (Redis). NULL for non-impersonation rows.';
COMMENT ON COLUMN common.audit_log_entry.act_reason IS
    'Denormalised reason from the impersonation session — captured at-rest for forensic queryability without needing to re-resolve the session record. NULL for non-impersonation rows.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS common.idx_audit_act_session_occurred;
DROP INDEX IF EXISTS common.idx_audit_act_operator_occurred;

ALTER TABLE common.audit_log_entry
    DROP COLUMN IF EXISTS act_reason,
    DROP COLUMN IF EXISTS act_session_id,
    DROP COLUMN IF EXISTS act_operator_id;

-- +goose StatementEnd
