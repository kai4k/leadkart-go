-- LeadKart Go - the tenant-wide pending queue of permission requests (ADR 0069 A6).
--
-- WHY. The approver queue used to read "approver_membership_id = caller", but that
-- column is only written when a request is DECIDED, so a pending request never
-- matched and the queue was always empty. The queue now reads the tenant's pending
-- requests (newest first, keyset-paginated) and keeps those the authority policy
-- lets the caller decide. This partial index serves that read; it is bounded to
-- pending rows, which are few. The old approver-pending index is left in place
-- (dropping an index is a separate, reversible decision).

-- +goose Up
CREATE INDEX idx_permission_requests_tenant_pending
    ON identity.permission_requests (tenant_id, created_at DESC, id DESC)
    WHERE state = 'pending';

-- +goose Down
DROP INDEX IF EXISTS identity.idx_permission_requests_tenant_pending;
