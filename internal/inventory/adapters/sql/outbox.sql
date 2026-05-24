-- Outbox queries — inventory.outbox is RLS+FORCE per ADR 0027 ("outbox
-- table doubles as audit log"). Insert happens inside the same tx as
-- aggregate state. Forwarder runs under platform-bypass to drain.

-- name: InsertOutboxEvent :exec
-- act_operator_id / act_session_id / act_reason carry impersonation
-- context (RFC 8693 act claim) per ADR 0056. NULL for non-impersonation
-- events; populated when the emitting handler ran under a scoped JWT.
INSERT INTO inventory.outbox (
    id, tenant_id, topic, payload, occurred_at,
    act_operator_id, act_session_id, act_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListUnforwardedOutboxEvents :many
-- Forwarder polls this. Caller MUST run under app.is_platform=true so
-- RLS policy inventory_outbox_select returns rows from every tenant.
--
-- FOR UPDATE SKIP LOCKED (Postgres 9.5+) lets multiple forwarder
-- replicas drain the outbox concurrently without double-publishing.
-- Mirror of identity's outbox.sql shape (Brandur Leach "Transactionally
-- staged job drains in Postgres" + river-queue canon).
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       forwarded, forwarded_at,
       act_operator_id, act_session_id, act_reason
FROM   inventory.outbox
WHERE  forwarded = false
ORDER  BY created_at
LIMIT  $1
FOR    UPDATE SKIP LOCKED;

-- name: MarkOutboxEventForwarded :exec
-- Forwarder writes this AFTER successful publish to the broker. Policy
-- inventory_outbox_modify requires app.is_platform=true.
UPDATE inventory.outbox
SET    forwarded    = true,
       forwarded_at = $2
WHERE  id = $1;
