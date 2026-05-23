-- LeadKart Go — Wave 9.1e — Time-bound overlay grants.
--
-- ADR 0055. Adds expires_at to identity.membership_permission_overrides
-- so approval-workflow approvals (the granted side of the overlay) can
-- carry a time-bound expiry. The PermissionResolver filters expired
-- entries at resolve time (no cron sweep at v0.2 scale — AWS STS /
-- Microsoft Entra ID JIT-access canon: caller-time filtering is
-- sufficient until measured pain demands otherwise).
--
-- expires_at NULL = perpetual (the existing default — every pre-9.1e
-- overlay row stays perpetual after this migration).
-- expires_at NOT NULL = approval-workflow grant; resolver drops it
-- once now() >= expires_at.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.membership_permission_overrides
    ADD COLUMN expires_at timestamptz NULL;

COMMENT ON COLUMN identity.membership_permission_overrides.expires_at IS
    'ADR 0055 — time-bound overlay grant. NULL = perpetual. Resolver filters expired entries at resolve time; no cron sweep at v0.2 scale.';

-- Forensic / future-cron-sweep index. Cheap (partial; only rows with
-- a set expiry). Phase 2 may add a job that periodically prunes rows
-- where expires_at < now() - 30 days; until then this index is for
-- ad-hoc "show me everyone's about-to-expire JIT grants" queries.
CREATE INDEX idx_membership_perm_overrides_expires_at
    ON identity.membership_permission_overrides (expires_at)
    WHERE expires_at IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS identity.idx_membership_perm_overrides_expires_at;
ALTER TABLE identity.membership_permission_overrides DROP COLUMN IF EXISTS expires_at;
-- +goose StatementEnd
