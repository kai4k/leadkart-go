-- companysettings module — commerce policy, history and GST rate queries
-- (ADR 0070). Tenant scoping is enforced by RLS (the adapter binds
-- app.tenant_id via the transactor); WHERE clauses key on tenant_id anyway.

-- name: GetCommercePolicy :one
SELECT tenant_id, version, policy, updated_at, updated_by_membership_id
FROM   companysettings.commerce_policy
WHERE  tenant_id = $1;

-- name: GetCommercePolicyForUpdate :one
-- Row-locks the tenant's policy for the UpdateFn transaction so concurrent
-- saves serialise; the version check then runs on the locked row.
SELECT tenant_id, version, policy, updated_at, updated_by_membership_id
FROM   companysettings.commerce_policy
WHERE  tenant_id = $1
FOR UPDATE;

-- name: InsertCommercePolicyIfAbsent :exec
-- Creates the row at version 0 (the defaults) so it can be locked. A
-- concurrent creator wins harmlessly.
INSERT INTO companysettings.commerce_policy (tenant_id, version, policy, updated_at)
VALUES ($1, 0, $2, now())
ON CONFLICT (tenant_id) DO NOTHING;

-- name: UpdateCommercePolicy :exec
UPDATE companysettings.commerce_policy SET
    version                  = $2,
    policy                   = $3,
    updated_at               = $4,
    updated_by_membership_id = $5
WHERE tenant_id = $1;

-- name: InsertPolicyHistory :exec
INSERT INTO companysettings.policy_history (
    tenant_id, version, policy, changed_keys, actor_membership_id,
    impersonator_operator_id, reason, changed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListGSTRates :many
SELECT id, tenant_id, rate_bps, label, confirmed_by, confirmed_on, active, created_at, created_by_membership_id
FROM   companysettings.gst_rates
WHERE  tenant_id = $1
ORDER  BY rate_bps, lower(label), id;

-- name: UpsertGSTRate :exec
-- rate_bps never changes on conflict: a different rate is a different row.
INSERT INTO companysettings.gst_rates (id, tenant_id, rate_bps, label, confirmed_by, confirmed_on, active, created_at, created_by_membership_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET
    label        = EXCLUDED.label,
    confirmed_by = EXCLUDED.confirmed_by,
    confirmed_on = EXCLUDED.confirmed_on,
    active       = EXCLUDED.active;
