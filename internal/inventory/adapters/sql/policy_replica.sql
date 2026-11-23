-- Inventory module — the event-fed replica of the company commerce policy
-- (ADR 0070). Tenant scoping is enforced by RLS (the adapter binds
-- app.tenant_id via the transactor); WHERE clauses key on tenant_id anyway.

-- name: GetPolicyReplica :one
SELECT tenant_id, version, policy, updated_at
FROM   inventory.policy_replica
WHERE  tenant_id = $1;

-- name: StorePolicyReplicaIfNewer :execrows
-- Stores the policy only when its version is newer than the stored one, so a
-- duplicate or reordered event changes nothing (0 rows affected).
INSERT INTO inventory.policy_replica (tenant_id, version, policy, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (tenant_id) DO UPDATE SET
    version    = EXCLUDED.version,
    policy     = EXCLUDED.policy,
    updated_at = now()
WHERE inventory.policy_replica.version < EXCLUDED.version;
