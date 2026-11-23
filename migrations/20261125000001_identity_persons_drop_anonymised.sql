-- Owner decision 2026-11-23: "We never delete any data. We keep it all. We just turn
-- the users IsActive true and false and show based on this." Privacy erasure is
-- deferred ("This is not required for now. We will have a look at it later for the
-- GDPR."); see ADR 0071, "Data is retained; erasure is deferred".
--
-- A person is now active or inactive and nothing else: the anonymised state
-- (is_anonymised, anonymised_at) goes. Nothing is live and no real data exists, so
-- there is no backfill and no compatibility path.
--
-- Three partial indexes are written against is_anonymised; they are replaced by the
-- same indexes on `WHERE is_active` before the columns go (the predicate would
-- otherwise drag the index down with the column, and the lookup paths would lose it).
--
-- Locks: ACCESS EXCLUSIVE on identity.persons for the index swap and the column drop
-- (catalogue-only; no rewrite). identity.persons is a directory, not a hot table.

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.idx_persons_active;
DROP INDEX IF EXISTS identity.idx_persons_search_trgm;
DROP INDEX IF EXISTS identity.idx_persons_email_active;

ALTER TABLE identity.persons
    DROP COLUMN is_anonymised,
    DROP COLUMN anonymised_at;

CREATE INDEX idx_persons_active ON identity.persons (id) WHERE is_active;

CREATE INDEX idx_persons_search_trgm
    ON identity.persons
    USING gin (
        (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name)) gin_trgm_ops
    )
    WHERE is_active;

CREATE INDEX idx_persons_email_active
    ON identity.persons (email)
    WHERE is_active;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.idx_persons_email_active;
DROP INDEX IF EXISTS identity.idx_persons_search_trgm;
DROP INDEX IF EXISTS identity.idx_persons_active;

ALTER TABLE identity.persons
    ADD COLUMN is_anonymised boolean     NOT NULL DEFAULT false,
    ADD COLUMN anonymised_at timestamptz NULL;

CREATE INDEX idx_persons_active ON identity.persons (id) WHERE is_active AND NOT is_anonymised;

CREATE INDEX idx_persons_search_trgm
    ON identity.persons
    USING gin (
        (lower(email) || ' ' || lower(first_name) || ' ' || lower(last_name)) gin_trgm_ops
    )
    WHERE is_active AND NOT is_anonymised;

CREATE INDEX idx_persons_email_active
    ON identity.persons (email)
    WHERE is_active AND NOT is_anonymised;

-- +goose StatementEnd
