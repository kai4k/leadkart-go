-- Owner decision 2026-11-23: "We never delete any data. We keep it all." A company
-- is pending, active or suspended and nothing else; it is never marked for deletion,
-- restored or deleted (ADR 0071, "Data is retained; erasure is deferred"). Nothing is
-- live and no real data exists, so there is no backfill and no compatibility path.
--
-- Removes the deletion lifecycle from identity.tenants: the columns
-- deletion_scheduled_at, deletion_reason and hard_deleted_at, and the two statuses
-- pending_deletion and deleted from the status CHECK.
--
-- The CHECK is narrowed WITHOUT touching any row: if a database still holds a tenant
-- in pending_deletion or deleted, the ADD CONSTRAINT fails and the migration stops,
-- loudly, instead of rewriting a company's state. Nothing in a deployed environment
-- can be in those states (the only route that set them is removed in the same
-- release and nothing is live).
--
-- Locks: ACCESS EXCLUSIVE on identity.tenants for the column drop and the constraint
-- swap (catalogue-only plus one validation scan of a one-row-per-company directory).

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.tenants
    DROP CONSTRAINT IF EXISTS tenants_status_check;

ALTER TABLE identity.tenants
    DROP COLUMN deletion_scheduled_at,
    DROP COLUMN deletion_reason,
    DROP COLUMN hard_deleted_at;

ALTER TABLE identity.tenants
    ADD CONSTRAINT tenants_status_check
        CHECK (status IN ('pending', 'active', 'suspended'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.tenants
    DROP CONSTRAINT IF EXISTS tenants_status_check;

ALTER TABLE identity.tenants
    ADD COLUMN deletion_scheduled_at timestamptz NULL,
    ADD COLUMN deletion_reason       text        NOT NULL DEFAULT '',
    ADD COLUMN hard_deleted_at       timestamptz NULL;

ALTER TABLE identity.tenants
    ADD CONSTRAINT tenants_status_check
        CHECK (status IN ('pending', 'active', 'suspended', 'pending_deletion', 'deleted'));

-- +goose StatementEnd
