-- LeadKart Go — Wave 9.2c — outbox act_* propagation columns (ADR 0056)
--
-- ADR 0056 — impersonation context propagation through the outbox →
-- Watermill subscriber boundary. Wave 4 (ADR 0045) shipped the
-- `audit_log_entry.act_*` columns + the JWT actor claim; Wave 4.1
-- (this migration's companion code) wires the propagation so the
-- subscriber-side AuditMiddleware can populate those audit columns
-- from a Watermill message.
--
-- Why columns and not JSONB metadata: outbox stays narrow + greppable,
-- partial indexes match the audit-table shape, and the forwarder maps
-- column → Watermill metadata 1:1 without a JSON parse on the hot
-- path. Same shape Microsoft's "Outbox pattern with .NET" guide uses
-- for cross-cutting headers + Brandur "events table" canon (well-named
-- columns over `metadata jsonb` for stable cross-cutting context).
--
-- Per RFC 8693 §4.1 actor claim + RFC 7515 design (named JWT claims
-- over `ext` blob) + OpenTelemetry Baggage (named keys, not opaque
-- metadata).
--
-- Non-impersonation rows leave these columns NULL — no data migration.
--
-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.outbox
    ADD COLUMN act_operator_id uuid NULL,
    ADD COLUMN act_session_id  uuid NULL,
    ADD COLUMN act_reason      text NULL;

COMMENT ON COLUMN identity.outbox.act_operator_id IS
    'RFC 8693 act.sub — operator who initiated the impersonation session under which this event was emitted. NULL for non-impersonation rows. Stamped by the outbox writer from authn.ClaimsFromContext(ctx).Act.Sub; the forwarder propagates onto Watermill message metadata so the subscriber-side AuditMiddleware can populate audit_log_entry.act_operator_id.';
COMMENT ON COLUMN identity.outbox.act_session_id IS
    'Impersonation session ID — same propagation path as act_operator_id. NULL for non-impersonation rows.';
COMMENT ON COLUMN identity.outbox.act_reason IS
    'Denormalised reason from the impersonation session — same propagation path. NULL for non-impersonation rows.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.outbox
    DROP COLUMN IF EXISTS act_reason,
    DROP COLUMN IF EXISTS act_session_id,
    DROP COLUMN IF EXISTS act_operator_id;

-- +goose StatementEnd
