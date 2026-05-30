-- Platform module lead-credits queries (ADR 0059). Optimistic-version
-- balance ledger; see internal/platform/adapters/lead_credit_repository_pg.go.

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
