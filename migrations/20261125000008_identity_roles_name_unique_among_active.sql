-- A deleted (inactive) role frees its name (owner ruling 2026-11-25, ADR 0073 addendum).
--
-- The web offers "Delete" on a role; behind the scenes it stays a state change (inactive, never
-- removed). The name is unique among ACTIVE roles of a company only, so a new "Area Manager" can be
-- created after the old one was deleted. Product SKUs are NOT changed: they stay unique over every
-- product, inactive ones included (old invoices print them). Drug schedule classes live in the
-- company policy document, not in a table: their label rule is in the domain, not an index.

-- +goose Up
-- +goose StatementBegin

DROP INDEX IF EXISTS identity.uq_roles_tenant_name;
CREATE UNIQUE INDEX uq_roles_tenant_name
    ON identity.roles (tenant_id, name) WHERE is_active;
-- GetRoleByTenantAndName: the active holder first, then the newest inactive one.
CREATE INDEX idx_roles_tenant_name_lookup
    ON identity.roles (tenant_id, name);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restoring the full index fails if an inactive role now shares a name with another role. Each such
-- inactive row keeps its data and takes a deterministic new name (its own id appended, within the 100
-- characters), as in 20261125000006.
UPDATE identity.roles d
SET    name = left(d.name, 100 - 37) || '~' || d.id::text
WHERE  NOT d.is_active
AND    EXISTS (SELECT 1 FROM identity.roles o
               WHERE o.tenant_id = d.tenant_id AND o.name = d.name AND o.id <> d.id);

DROP INDEX IF EXISTS identity.idx_roles_tenant_name_lookup;
DROP INDEX IF EXISTS identity.uq_roles_tenant_name;
CREATE UNIQUE INDEX uq_roles_tenant_name
    ON identity.roles (tenant_id, name);

-- +goose StatementEnd
