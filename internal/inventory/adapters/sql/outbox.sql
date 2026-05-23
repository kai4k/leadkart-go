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
