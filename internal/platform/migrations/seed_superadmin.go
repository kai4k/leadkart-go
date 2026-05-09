// Package migrations holds Go-shaped goose migrations alongside the
// SQL files in `migrations/`. Each file in this package registers
// itself with the global goose registry via init(); the cmd/migrate
// binary blank-imports the package once so the registrations fire
// before goose collects migrations from the filesystem.
//
// Why a separate package (not migrations/seed.go): the .sql migrations
// are tooling artefacts that goose parses verbatim — putting Go files
// in the same directory mixes a build target into the SQL fixture.
// Convention used by Brandur Leach + the goose project examples:
// SQL migrations stay in migrations/, Go-shaped ones live in
// internal/platform/migrations/ and are registered programmatically.
package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
)

// Per ADR migration-version doctrine: filename-encoded versions are
// the source of truth. The virtual filename below carries the
// version; goose parses the leading timestamp.
const seedSuperAdminFilename = "20260507000008_seed_superadmin.go"

// Env-var contract — both must be present for the migration to seed.
// Missing either → skip with a WARN log so dev environments can still
// boot. Production deploys MUST set both at apply-time (operator
// supplies the bootstrap pair, then rotates immediately via the
// platform admin UI per security canon).
const (
	envSuperAdminEmail     = "LEADKART_SUPERADMIN__EMAIL"
	envSuperAdminPassword  = "LEADKART_SUPERADMIN__PASSWORD"
	envSuperAdminFirstName = "LEADKART_SUPERADMIN__FIRST_NAME" // optional; default "Platform"
	envSuperAdminLastName  = "LEADKART_SUPERADMIN__LAST_NAME"  // optional; default "SuperAdmin"
)

// Fixed conventions per multi-tenancy.md "SuperUser god-mode" + the
// AskUserQuestion answer "platform" (short + memorable slug).
const (
	platformTenantSlug = "platform"
	platformLegalName  = "LeadKart Platform"
	platformDispName   = "Platform"

	superAdminRoleName       = "SuperAdmin"
	superAdminHierarchyLevel = 0 // top of tier (lower number = higher authority per role.HierarchyLevel*)
)

func init() {
	goose.AddNamedMigrationContext(seedSuperAdminFilename, upSeedSuperAdmin, downSeedSuperAdmin)
}

// upSeedSuperAdmin idempotently provisions the platform tenant + the
// SuperAdmin role + the bootstrap operator Person + their Membership +
// the role assignment. Re-running the migration on an already-seeded
// database is a no-op (every INSERT carries ON CONFLICT DO NOTHING +
// IDs are looked up via UNIQUE keys, never re-generated).
//
// Called inside goose's per-migration tx. The connecting role
// (POSTGRES_USER) is a database superuser, so the FORCE-RLS policies
// on tenant-scoped tables (roles, memberships, role_assignments) are
// bypassed for the duration. We do NOT toggle app.is_platform here:
// the migration is a one-shot ops action, not the runtime data path,
// and superuser bypass is the simpler, more honest contract.
func upSeedSuperAdmin(ctx context.Context, tx *sql.Tx) error {
	log := slog.Default()

	email := strings.TrimSpace(os.Getenv(envSuperAdminEmail))
	password := os.Getenv(envSuperAdminPassword)
	if email == "" || password == "" {
		log.WarnContext(ctx, "SuperAdmin seed skipped: env vars not set",
			"required", []string{envSuperAdminEmail, envSuperAdminPassword},
		)
		return nil
	}

	firstName := strings.TrimSpace(os.Getenv(envSuperAdminFirstName))
	if firstName == "" {
		firstName = "Platform"
	}
	lastName := strings.TrimSpace(os.Getenv(envSuperAdminLastName))
	if lastName == "" {
		lastName = "SuperAdmin"
	}

	now := time.Now().UTC()

	// 1. Platform tenant — ON CONFLICT (slug) keeps the existing row's
	//    id when re-running. RETURNING id surfaces it whether new or
	//    existing.
	tenantID := ids.NewV7().String()
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO identity.tenants (
		    id, slug, legal_name, display_name, admin_email, status,
		    created_at, activated_at
		) VALUES ($1, $2, $3, $4, $5, 'active', $6, $6)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id
	`, tenantID, platformTenantSlug, platformLegalName, platformDispName, email, now).
		Scan(&tenantID); err != nil {
		return fmt.Errorf("seed superadmin: upsert platform tenant: %w", err)
	}

	// 2. SuperAdmin Person — globally-unique by email (UNIQUE constraint).
	//    Hash via Argon2id at the OWASP 2025 floor (m=19MiB, t=2, p=1).
	hash, err := argon2.Hash(password)
	if err != nil {
		return fmt.Errorf("seed superadmin: hash password: %w", err)
	}
	personID := ids.NewV7().String()
	stamp := ids.NewV7().String()
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO identity.persons (
		    id, email, first_name, last_name,
		    password_hash, security_stamp, is_active, is_anonymised, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, true, false, $7)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id
	`, personID, email, firstName, lastName, hash, stamp, now).
		Scan(&personID); err != nil {
		return fmt.Errorf("seed superadmin: upsert person: %w", err)
	}

	// 3. SuperAdmin Role under the platform tenant — UNIQUE
	//    (tenant_id, name) WHERE NOT is_deleted is the idempotency key.
	//    Permissions array is empty: the resolver short-circuits on
	//    is_super_admin=true (PermissionResolver.HasAll returns true
	//    unconditionally for super-admin Memberships), so listing
	//    permissions here would be redundant + a maintenance hazard.
	roleID := ids.NewV7().String()
	if err := tx.QueryRowContext(ctx, `
		WITH ins AS (
		    INSERT INTO identity.roles (
		        id, tenant_id, name, is_system_default, is_super_admin,
		        hierarchy_level, permissions, created_at
		    ) VALUES ($1, $2, $3, true, true, $4, '[]'::jsonb, $5)
		    ON CONFLICT (tenant_id, name) WHERE NOT is_deleted DO NOTHING
		    RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM identity.roles
		WHERE  tenant_id = $2 AND name = $3 AND NOT is_deleted
		LIMIT  1
	`, roleID, tenantID, superAdminRoleName, superAdminHierarchyLevel, now).
		Scan(&roleID); err != nil {
		return fmt.Errorf("seed superadmin: upsert role: %w", err)
	}

	// 4. Membership — single-Active-Membership invariant means the
	//    SuperAdmin Person can hold AT MOST one active row. Idempotency
	//    via UNIQUE (person_id, tenant_id).
	membershipID := ids.NewV7().String()
	if err := tx.QueryRowContext(ctx, `
		WITH ins AS (
		    INSERT INTO identity.tenant_memberships (
		        id, person_id, tenant_id, status, joined_at
		    ) VALUES ($1, $2, $3, 'active', $4)
		    ON CONFLICT (person_id, tenant_id) DO NOTHING
		    RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM identity.tenant_memberships
		WHERE  person_id = $2 AND tenant_id = $3
		LIMIT  1
	`, membershipID, personID, tenantID, now).
		Scan(&membershipID); err != nil {
		return fmt.Errorf("seed superadmin: upsert membership: %w", err)
	}

	// 5. Role assignment — composite PK (membership_id, role_id) is
	//    the idempotency key; ON CONFLICT DO NOTHING covers re-runs.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity.role_assignments (
		    membership_id, role_id, tenant_id, assigned_at
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (membership_id, role_id) DO NOTHING
	`, membershipID, roleID, tenantID, now); err != nil {
		return fmt.Errorf("seed superadmin: upsert role assignment: %w", err)
	}

	log.InfoContext(ctx, "SuperAdmin seeded",
		"tenant_id", tenantID,
		"person_id", personID,
		"membership_id", membershipID,
		"role_id", roleID,
		"email", email,
	)
	return nil
}

// downSeedSuperAdmin reverses the seed by deterministic markers
// (platform slug + admin email). Drops the role assignment, role,
// membership, person, and platform tenant — in FK-safe order.
//
// In production this is rarely run — once SuperAdmin exists it gets
// rotated, not deleted. The path exists so `goose down` works
// symmetrically against a freshly-seeded dev database.
func downSeedSuperAdmin(ctx context.Context, tx *sql.Tx) error {
	email := strings.TrimSpace(os.Getenv(envSuperAdminEmail))
	if email == "" {
		// Nothing to undo if the up path was a no-op.
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity.role_assignments ra
		USING  identity.tenant_memberships m, identity.persons p, identity.tenants t
		WHERE  ra.membership_id = m.id
		  AND  m.person_id = p.id
		  AND  m.tenant_id = t.id
		  AND  p.email = $1
		  AND  t.slug  = $2
	`, email, platformTenantSlug); err != nil {
		return fmt.Errorf("seed superadmin (down): delete role_assignments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity.roles
		WHERE  tenant_id = (SELECT id FROM identity.tenants WHERE slug = $1)
		  AND  name = $2
	`, platformTenantSlug, superAdminRoleName); err != nil {
		return fmt.Errorf("seed superadmin (down): delete role: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity.tenant_memberships m
		USING  identity.persons p, identity.tenants t
		WHERE  m.person_id = p.id
		  AND  m.tenant_id = t.id
		  AND  p.email = $1
		  AND  t.slug  = $2
	`, email, platformTenantSlug); err != nil {
		return fmt.Errorf("seed superadmin (down): delete membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity.persons WHERE email = $1
	`, email); err != nil {
		return fmt.Errorf("seed superadmin (down): delete person: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM identity.tenants WHERE slug = $1
	`, platformTenantSlug); err != nil {
		return fmt.Errorf("seed superadmin (down): delete tenant: %w", err)
	}
	return nil
}
