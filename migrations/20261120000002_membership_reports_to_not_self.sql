-- LeadKart Go - a membership never reports to itself (ADR 0069 H4).
--
-- WHY. reports_to is a uuid, so any spelling of the member's own id (upper case,
-- urn:uuid:, braces) collapses to the same value in the column. Until the
-- commands canonicalised ids, a string comparison in the aggregate missed those
-- spellings and a member could be stored as its own manager. The command now
-- rejects the self link and walks the proposed manager's chain for cycles under
-- the tenant authority lock; this CHECK is the durable backstop for the
-- self-loop. Longer cycles are prevented by the command only (a recursive-CTE
-- trigger, like ADR 0058's for role edges, is follow-up).
--
-- EXISTING DATA. A stored self-loop (reports_to = id) is repaired before the
-- constraint is added: reports_to goes back to NULL (a member with itself as
-- manager has no manager in any meaningful sense, and approval already refuses
-- self-approval). Every repaired row is recorded in common.audit_log_entry
-- (system actor, the tenant, the membership id), which is also Down's ledger:
-- Down drops the constraint and puts each recorded self-loop back, but only
-- while the membership has no manager (a link set since is left alone).

-- +goose Up
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

WITH fixed AS (
    UPDATE identity.tenant_memberships
    SET    reports_to = NULL
    WHERE  reports_to = id
    RETURNING id, tenant_id
)
INSERT INTO common.audit_log_entry
    (id, action, user_id, tenant_id, correlation_id, occurred_at_utc, duration_ms, succeeded, payload)
SELECT gen_random_uuid(), 'identity.membership.migrated', NULL, f.tenant_id, NULL, now(), 0, true,
       jsonb_build_object(
           'migration', '20261120000002_membership_reports_to_not_self',
           'reason', 'ADR 0069: a membership cannot report to itself; self-loop cleared',
           'membership_id', f.id,
           'before', jsonb_build_object('reports_to', f.id),
           'after',  jsonb_build_object('reports_to', NULL))
FROM   fixed f;

ALTER TABLE identity.tenant_memberships
    ADD CONSTRAINT membership_reports_to_not_self CHECK (reports_to IS NULL OR reports_to <> id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT set_config('app.is_platform', 'true', true);

ALTER TABLE identity.tenant_memberships DROP CONSTRAINT IF EXISTS membership_reports_to_not_self;

UPDATE identity.tenant_memberships m
SET    reports_to = m.id
FROM   common.audit_log_entry a
WHERE  a.action = 'identity.membership.migrated'
  AND  a.payload ->> 'migration' = '20261120000002_membership_reports_to_not_self'
  AND  (a.payload ->> 'membership_id')::uuid = m.id
  AND  m.reports_to IS NULL;
-- +goose StatementEnd
