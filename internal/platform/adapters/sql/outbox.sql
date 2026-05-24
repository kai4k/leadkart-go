-- Platform module outbox queries. Same shape as identity.outbox per
-- ADR 0008 + 0027 + ADR 0059.

-- name: InsertPlatformOutboxEvent :exec
INSERT INTO platform.outbox (
    id, tenant_id, topic, payload, occurred_at,
    act_operator_id, act_session_id, act_reason
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListUnforwardedPlatformOutboxEvents :many
-- Forwarder polls this under platform-bypass. FOR UPDATE SKIP LOCKED
-- lets multiple forwarder replicas drain concurrently without
-- double-publishing (same canonical shape as identity's forwarder).
SELECT id, tenant_id, topic, payload, occurred_at, created_at,
       forwarded, forwarded_at,
       act_operator_id, act_session_id, act_reason
FROM   platform.outbox
WHERE  forwarded = false
ORDER  BY created_at
LIMIT  $1
FOR    UPDATE SKIP LOCKED;

-- name: MarkPlatformOutboxEventForwarded :exec
UPDATE platform.outbox
SET    forwarded    = true,
       forwarded_at = $2
WHERE  id = $1;

-- name: InsertLeadCredit :exec
-- Inserts a fresh row with version=1 so post-INSERT reads return
-- Version=1. The repository's optimistic-version UPDATE path can
-- then unambiguously detect "fresh aggregate, no DB row" by checking
-- in-memory Version==0 (only NewForTenant emits 0; every loaded
-- aggregate carries >=1).
INSERT INTO platform.lead_credits (
    tenant_id, balance, version, created_at, updated_at
) VALUES (
    $1, $2, 1, $3, $4
);

-- name: GetLeadCredit :one
SELECT tenant_id, balance, version, created_at, updated_at
FROM   platform.lead_credits
WHERE  tenant_id = $1;

-- name: UpdateLeadCreditWithVersion :execrows
-- Optimistic-version UPDATE per ADR 0059. Returns rows-affected;
-- caller treats 0 as ErrConflict + retries.
UPDATE platform.lead_credits
SET    balance    = sqlc.arg(new_balance)::bigint,
       version    = sqlc.arg(expected_version)::bigint + 1,
       updated_at = sqlc.arg(updated_at)::timestamptz
WHERE  tenant_id = sqlc.arg(tenant_id)::uuid
  AND  version   = sqlc.arg(expected_version)::bigint;
