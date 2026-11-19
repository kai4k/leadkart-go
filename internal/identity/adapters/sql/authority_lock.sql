-- name: LockTenantAuthority :exec
-- ADR 0069: transaction-scoped advisory lock serialising every authority-
-- changing write within one tenant (role / override / status / manager /
-- approval). Released automatically at commit or rollback.
SELECT pg_advisory_xact_lock(hashtextextended('identity.authority:' || sqlc.arg(tenant_id)::uuid::text, 0));
