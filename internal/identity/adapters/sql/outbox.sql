-- Outbox queries — identity.outbox is RLS+FORCE per ADR 0027 ("outbox
-- table doubles as audit log"). Insert happens inside the same tx as
-- aggregate state (Brandur "events table" pattern). Forwarder runs
-- under platform-bypass to drain.

-- name: InsertOutboxEvent :exec
-- act_operator_id / act_session_id / act_reason carry impersonation
-- context (RFC 8693 act claim) per ADR 0056. NULL for non-impersonation
-- events; populated when the emitting handler ran under a scoped JWT.
INSERT INTO identity.outbox (
    id, tenant_id, topic, payload, occurred_at,
    act_operator_id, act_session_id, act_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListUnforwardedOutboxEvents :many
-- Forwarder polls this. Caller MUST run under app.is_platform=true so
-- RLS policy outbox_select returns rows from every tenant.
--
-- FOR UPDATE SKIP LOCKED (Postgres 9.5+) lets multiple forwarder
-- replicas drain the outbox concurrently without double-publishing.
-- During a Kubernetes rolling deploy the old + new pod overlap; a
-- plain SELECT here would let both pick up the same rows + both
-- publish + both UPDATE forwarded=true (one win, one duplicate
-- downstream). SKIP LOCKED makes Postgres skip rows already row-
-- locked by a sibling tx, so each in-flight forwarder sees a
-- disjoint slice. Canonical Watermill SQL outbox shape +
-- river-queue + Brandur Leach "Transactionally staged job drains
-- in Postgres" use the same primitive.
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       forwarded, forwarded_at,
       act_operator_id, act_session_id, act_reason
FROM   identity.outbox
WHERE  forwarded = false
ORDER  BY created_at, id
LIMIT  $1
FOR    UPDATE SKIP LOCKED;

-- name: MarkOutboxEventForwarded :exec
-- Forwarder writes this AFTER successful publish to the broker. Policy
-- outbox_modify requires app.is_platform=true.
UPDATE identity.outbox
SET    forwarded    = true,
       forwarded_at = $2
WHERE  id = $1;

-- name: ListAuditEventsForTenant :many
-- Per-tenant audit query (the outbox doubling as audit log per ADR 0027).
-- Runs under tenant scope; RLS filters automatically.
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       forwarded, forwarded_at,
       act_operator_id, act_session_id, act_reason
FROM   identity.outbox
WHERE  topic = $1
  AND  occurred_at >= $2
ORDER  BY occurred_at DESC, id DESC
LIMIT  $3;
