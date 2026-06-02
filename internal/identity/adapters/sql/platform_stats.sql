-- Platform operator-dashboard COUNT(*) rollups. Run under TxScopePlatform
-- so the RLS-scoped tenant_memberships count sees every tenant's rows.
--
-- Each method is collapsed to ONE round-trip via scalar subqueries
-- (was N sequential SELECT COUNT(*) round-trips before the sqlc port).
-- Counts/filters preserve the exact semantics of the prior hand-rolled
-- queries.

-- name: PlatformStatsBase :one
SELECT
    (SELECT count(*) FROM identity.tenants)                                          AS tenants_total,
    (SELECT count(*) FROM identity.tenants WHERE status = 'active')                  AS tenants_active,
    (SELECT count(*) FROM identity.tenants WHERE status = 'suspended')               AS tenants_suspended,
    (SELECT count(*) FROM identity.persons WHERE NOT is_anonymised)                  AS persons_total,
    (SELECT count(*) FROM identity.tenant_memberships WHERE status = 'active')        AS memberships_active;

-- name: PlatformStatsDeltas :one
-- "new rows in interval" rollup. window is a Postgres interval string
-- ("24 hours" / "7 days" / "30 days"); the closed-set label validator
-- lives in the app layer.
SELECT
    (SELECT count(*) FROM identity.tenants
        WHERE created_at > now() - sqlc.arg(win)::interval)                        AS tenants_total,
    (SELECT count(*) FROM identity.tenants
        WHERE activated_at > now() - sqlc.arg(win)::interval)                      AS tenants_active,
    (SELECT count(*) FROM identity.persons
        WHERE created_at > now() - sqlc.arg(win)::interval
          AND NOT is_anonymised)                                                     AS persons_total,
    (SELECT count(*) FROM identity.tenant_memberships
        WHERE joined_at > now() - sqlc.arg(win)::interval
          AND status = 'active')                                                     AS memberships_active;
