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
-- Forwarder polling query.
--
-- Ordering is created_at then id — the UUIDv7 id is the tiebreaker so
-- events that commit with the same created_at still drain in causal order.
--
-- FOR UPDATE SKIP LOCKED: lets multiple forwarder replicas drain
-- concurrently without double-publishing — each replica locks a disjoint
-- slice and skips rows another replica already holds. Mirrors the
-- identity/platform/inventory drains; without it a rolling deploy with
-- two forwarders republishes every row.
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       act_operator_id, act_session_id, act_reason
FROM   crm.outbox
WHERE  NOT forwarded
ORDER  BY created_at ASC, id ASC
LIMIT  $1
FOR    UPDATE SKIP LOCKED;

-- name: MarkCRMEventForwarded :exec
UPDATE crm.outbox
SET    forwarded = true,
       forwarded_at = $2
WHERE  id = $1;
