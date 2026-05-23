-- LeadKart Go — Wave 9.1e — Permission-elevation approval workflow.
--
-- ADR 0055. Adds the permission_requests aggregate's storage. A
-- Membership requests an elevated permission; their direct manager
-- (per tenant_memberships.reports_to) approves or denies. On approval
-- the requested permission lands on the membership's overlay as a
-- TIME-BOUND grant (migration 20260523000004 adds expires_at to
-- membership_permission_overrides).
--
-- State machine (column `state`):
--   pending → approved (success) | denied | cancelled (by requester)
--   approved is terminal from the workflow's POV; the actual grant
--   lives as an ExpiresAt-bounded entry on the Membership overlay.
--   No "expired" workflow state — the grant on the Membership expires;
--   the Request row stays `approved` for audit history.
--
-- Invariants:
--   - At most one PENDING request per (requester_membership_id,
--     permission_constant). Partial unique index enforces.
--   - duration_days bounded [1, 90] per ADR 0055 closed-set guard.
--   - reason ≥ 10 chars (matches impersonation reason length floor +
--     DPDP §12 / SOC2 CC4.1 audit canon).
--   - tenant_id matches the requester membership's tenant_id (FK
--     cascades; RLS scopes).
--   - approver_membership_id != requester_membership_id (DB-level
--     CHECK; domain ChangeState already enforces).
--
-- RLS+FORCE per multi-tenancy.md canon — tenant-scoped table.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE identity.permission_requests (
    id                       uuid        NOT NULL PRIMARY KEY,
    tenant_id                uuid        NOT NULL REFERENCES identity.tenants(id) ON DELETE CASCADE,
    requester_membership_id  uuid        NOT NULL REFERENCES identity.tenant_memberships(id) ON DELETE CASCADE,
    permission_constant      text        NOT NULL,
    duration_days            int         NOT NULL CHECK (duration_days BETWEEN 1 AND 90),
    reason                   text        NOT NULL CHECK (length(reason) >= 10 AND length(reason) <= 1024),
    state                    text        NOT NULL CHECK (state IN ('pending','approved','denied','cancelled')),
    approver_membership_id   uuid        NULL REFERENCES identity.tenant_memberships(id) ON DELETE SET NULL,
    decided_at               timestamptz NULL,
    decision_reason          text        NULL CHECK (decision_reason IS NULL OR length(decision_reason) <= 1024),
    granted_override_id      uuid        NULL,
    expires_at               timestamptz NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    -- Self-approval blocked at the schema level. Domain Approve()
    -- already rejects this; defense-in-depth alongside the domain
    -- guard per ADR 0055.
    CONSTRAINT permission_requests_no_self_approval
        CHECK (approver_membership_id IS NULL OR approver_membership_id <> requester_membership_id)
);

COMMENT ON TABLE identity.permission_requests IS
    'ADR 0055 — permission-elevation approval workflow. Stores Requested → Approved/Denied/Cancelled history; approved grants land on identity.membership_permission_overrides with expires_at bounded.';

-- At-most-one-pending-per-(membership, permission) invariant. ADR 0055
-- canonical example of partial-unique-index-as-invariant per Brandur
-- "Postgres unique indexes for distributed locks".
CREATE UNIQUE INDEX uq_permission_requests_pending
    ON identity.permission_requests (requester_membership_id, permission_constant)
    WHERE state = 'pending';

-- Approver-side queue index — "show me everything waiting for MY
-- decision" hits this. Partial-index keeps the queue cheap.
CREATE INDEX idx_permission_requests_approver_pending
    ON identity.permission_requests (approver_membership_id, created_at DESC)
    WHERE state = 'pending';

-- Requester-side history index — "show me MY request history" hits this.
CREATE INDEX idx_permission_requests_requester_state_created
    ON identity.permission_requests (requester_membership_id, state, created_at DESC);

-- RLS — tenant-scoped per multi-tenancy.md. The platform-bypass
-- escape hatch lives in pg.TxScopePlatform (sets app.is_platform=true);
-- the BYPASSED rows are visible only to platform-tier transactions.
ALTER TABLE identity.permission_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.permission_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_select ON identity.permission_requests FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_insert ON identity.permission_requests FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_update ON identity.permission_requests FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid
                OR current_setting('app.is_platform', true)::boolean IS TRUE);
CREATE POLICY tenant_isolation_delete ON identity.permission_requests FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid
           OR current_setting('app.is_platform', true)::boolean IS TRUE);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS identity.permission_requests;
-- +goose StatementEnd
