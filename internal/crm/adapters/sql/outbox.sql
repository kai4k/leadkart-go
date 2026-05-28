-- CRM outbox queries — crm.outbox is RLS+FORCE; mirror of
-- identity.outbox + platform.outbox. Per ADR 0008 + 0027 + 0056.
--
-- The forwarder polls via [SelectUnforwardedCRMEvents]; per-handler
-- writes use [InsertOutboxEvent] inside the same tx as the aggregate
-- mutation; the forwarder marks rows via [MarkCRMEventForwarded] after
-- successful broker publish.

-- name: InsertCRMOutboxEvent :exec
INSERT INTO crm.outbox (
    id, tenant_id, topic, payload, occurred_at,
    act_operator_id, act_session_id, act_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: SelectUnforwardedCRMEvents :many
-- Forwarder polling query. ORDER BY (created_at ASC, id ASC) for FIFO
-- publish order — id is the canonical UUIDv7 tiebreaker when two
-- events commit with the same created_at (same-tx pair like
-- TenantRegistered → TenantActivated). Without the tiebreaker
-- Postgres returns ties in undefined order → consumers see events
-- out of causal order. Per Brandur Leach "Transactionally Staged
-- Job Drains" + Watermill SQL outbox + ADR 0027 + the
-- TestArch_OutboxSelectsOrderByMonotonicTiebreaker gate.
-- LIMIT is supplied per-call so the forwarder can batch without
-- runaway memory under outbox backlog.
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       act_operator_id, act_session_id, act_reason
FROM   crm.outbox
WHERE  NOT forwarded
ORDER  BY created_at ASC, id ASC
LIMIT  $1;

-- name: MarkCRMEventForwarded :exec
UPDATE crm.outbox
SET    forwarded = true,
       forwarded_at = $2
WHERE  id = $1;
