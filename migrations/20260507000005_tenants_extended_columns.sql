-- LeadKart Go — Tenant aggregate persistence catch-up.
-- ADR 0003 (state-based persistence — every aggregate field must
-- round-trip through the adapter) + ADR 0001 (modular monolith —
-- identity owns its schema).
--
-- Closes the schema gap that the A.4 work exposed: the Tenant
-- aggregate carries Statutory + AdminContact + Settings +
-- DisplayPreferences + deletion-grace fields, but identity.tenants
-- only persisted the original 9 columns. Updates went through unit
-- tests (fakes preserve in-memory state) but did NOT round-trip
-- through the real adapter — the columns simply didn't exist.
--
-- Plus: extend the status CHECK to include the lifecycle states
-- StatusPendingDeletion + StatusDeleted that A.4 added to the
-- aggregate enum.
--
-- Per `data-retention.md` "Tenant deletion saga" + `coding-
-- standards.md` "Statutory composite VO" + canon for retention
-- columns + display preferences.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE identity.tenants
    DROP CONSTRAINT IF EXISTS tenants_status_check;

ALTER TABLE identity.tenants
    ADD CONSTRAINT tenants_status_check
        CHECK (status IN ('pending', 'active', 'suspended', 'pending_deletion', 'deleted'));

ALTER TABLE identity.tenants
    -- Statutory composite VO. Each may be empty until the tenant
    -- declares it post-onboarding. NULL is the "not declared" signal.
    ADD COLUMN gst_number          text NOT NULL DEFAULT '',
    ADD COLUMN pan_number          text NOT NULL DEFAULT '',
    ADD COLUMN drug_licence_number text NOT NULL DEFAULT '',

    -- AdminContact. Phone in E.164; postal address Indian-style with
    -- 6-digit pincode + state + state code.
    ADD COLUMN admin_phone               text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_street      text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_city        text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_district    text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_state       text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_state_code  text NOT NULL DEFAULT '',
    ADD COLUMN admin_address_pincode     text NOT NULL DEFAULT '',

    -- Settings.PasswordPolicy. NIST SP 800-63B floors enforced by
    -- the aggregate VO; schema accepts any non-negative int.
    ADD COLUMN password_min_length          integer NOT NULL DEFAULT 0,
    ADD COLUMN password_require_uppercase   boolean NOT NULL DEFAULT false,
    ADD COLUMN password_require_lowercase   boolean NOT NULL DEFAULT false,
    ADD COLUMN password_require_digit       boolean NOT NULL DEFAULT false,
    ADD COLUMN password_require_symbol      boolean NOT NULL DEFAULT false,
    ADD COLUMN password_max_failed_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN password_lockout_minutes     integer NOT NULL DEFAULT 0,

    -- DisplayPreferences. BCP 47 locale + IANA tz + ISO 4217.
    ADD COLUMN locale      text NOT NULL DEFAULT '',
    ADD COLUMN time_zone   text NOT NULL DEFAULT '',
    ADD COLUMN date_format text NOT NULL DEFAULT '',
    ADD COLUMN currency    text NOT NULL DEFAULT '',

    -- Tenant deletion saga (data-retention.md "Tenant deletion saga").
    ADD COLUMN deletion_scheduled_at timestamptz NULL,
    ADD COLUMN deletion_reason       text        NOT NULL DEFAULT '',
    ADD COLUMN hard_deleted_at       timestamptz NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE identity.tenants
    DROP COLUMN IF EXISTS hard_deleted_at,
    DROP COLUMN IF EXISTS deletion_reason,
    DROP COLUMN IF EXISTS deletion_scheduled_at,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS date_format,
    DROP COLUMN IF EXISTS time_zone,
    DROP COLUMN IF EXISTS locale,
    DROP COLUMN IF EXISTS password_lockout_minutes,
    DROP COLUMN IF EXISTS password_max_failed_attempts,
    DROP COLUMN IF EXISTS password_require_symbol,
    DROP COLUMN IF EXISTS password_require_digit,
    DROP COLUMN IF EXISTS password_require_lowercase,
    DROP COLUMN IF EXISTS password_require_uppercase,
    DROP COLUMN IF EXISTS password_min_length,
    DROP COLUMN IF EXISTS admin_address_pincode,
    DROP COLUMN IF EXISTS admin_address_state_code,
    DROP COLUMN IF EXISTS admin_address_state,
    DROP COLUMN IF EXISTS admin_address_district,
    DROP COLUMN IF EXISTS admin_address_city,
    DROP COLUMN IF EXISTS admin_address_street,
    DROP COLUMN IF EXISTS admin_phone,
    DROP COLUMN IF EXISTS drug_licence_number,
    DROP COLUMN IF EXISTS pan_number,
    DROP COLUMN IF EXISTS gst_number;

ALTER TABLE identity.tenants
    DROP CONSTRAINT IF EXISTS tenants_status_check;

ALTER TABLE identity.tenants
    ADD CONSTRAINT tenants_status_check
        CHECK (status IN ('pending', 'active', 'suspended'));

-- +goose StatementEnd
