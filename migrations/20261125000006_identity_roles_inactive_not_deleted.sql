-- Roles go Inactive, never deleted (owner ruling 2026-11-24, ADR 0073).
--
-- is_active replaces is_deleted / deleted_at / deleted_by. A role soft-deleted before this
-- migration becomes INACTIVE (an inactive role confers nothing, exactly as a deleted one did), never
-- active again: the permissions it carries do not come back. Role names are unique over every role
-- of the company, inactive ones included (a name is no longer freed by "deleting").

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.roles ADD COLUMN is_active boolean NOT NULL DEFAULT true;
UPDATE identity.roles SET is_active = false WHERE is_deleted;

DROP INDEX IF EXISTS identity.uq_roles_tenant_name;
DROP INDEX IF EXISTS identity.idx_roles_super_admin;
DROP INDEX IF EXISTS identity.idx_roles_tenant_created_keyset;

ALTER TABLE identity.roles
    DROP COLUMN is_deleted,
    DROP COLUMN deleted_at,
    DROP COLUMN deleted_by;

CREATE UNIQUE INDEX uq_roles_tenant_name
    ON identity.roles (tenant_id, name);
CREATE INDEX idx_roles_super_admin
    ON identity.roles (id) WHERE is_super_admin AND is_active;
CREATE INDEX idx_roles_tenant_created_keyset
    ON identity.roles (tenant_id, created_at DESC, id DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.idx_roles_tenant_created_keyset;
DROP INDEX IF EXISTS identity.idx_roles_super_admin;
DROP INDEX IF EXISTS identity.uq_roles_tenant_name;

ALTER TABLE identity.roles
    ADD COLUMN is_deleted boolean     NOT NULL DEFAULT false,
    ADD COLUMN deleted_at timestamptz NULL,
    ADD COLUMN deleted_by text        NULL;
-- An inactive role goes back as a soft-deleted one: it still confers nothing.
UPDATE identity.roles SET is_deleted = true, deleted_at = now() WHERE NOT is_active;

ALTER TABLE identity.roles DROP COLUMN is_active;

CREATE UNIQUE INDEX uq_roles_tenant_name
    ON identity.roles (tenant_id, name) WHERE NOT is_deleted;
CREATE INDEX idx_roles_super_admin
    ON identity.roles (id) WHERE is_super_admin AND NOT is_deleted;
CREATE INDEX idx_roles_tenant_created_keyset
    ON identity.roles (tenant_id, created_at DESC, id DESC) WHERE NOT is_deleted;

-- +goose StatementEnd
