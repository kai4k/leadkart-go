// Package main is the LeadKart bootstrap CLI.
//
// One-shot tool that idempotently provisions the platform-tier rows
// every fresh environment needs: the platform tenant, the SuperAdmin
// role, the bootstrap operator Person, their Membership, and the
// Person↔Role assignment.
//
// Why this lives outside of `migrations/`: per the canon split
// (Stripe / Plaid / GitHub), schema migrations stay pure-SQL +
// data-free. Data seeding goes through a CLI run once per
// environment by the operator (or once-per-env in CI/CD). This
// keeps migrations deterministic + reversible, and keeps secret
// reads (the SuperAdmin password) out of the schema-management path.
//
// Idempotency: every INSERT carries an ON CONFLICT branch keyed on
// the natural UNIQUE constraint, so re-running against an already-
// bootstrapped database is a safe no-op. Tools like `kubectl run`
// or `docker compose run --rm` can call this on every deploy
// without harm.
//
// Required env (skipped with WARN if absent):
//
//	LEADKART_POSTGRES__DSN              postgres connection string
//	LEADKART_SUPERADMIN__EMAIL          login email
//	LEADKART_SUPERADMIN__PASSWORD       plaintext; hashed via Argon2id
//
// Optional env:
//
//	LEADKART_SUPERADMIN__FIRST_NAME     default "Platform"
//	LEADKART_SUPERADMIN__LAST_NAME      default "SuperAdmin"
//
// Usage:
//
//	bootstrap                     # default: provision SuperAdmin
//	bootstrap --status            # report what's already provisioned (no writes)
//
// Production deploys:
//
//	1. Run after `migrate up` succeeds, before the api Deployment rolls.
//	2. Run ONCE per environment OR every deploy (idempotent either way).
//	3. Secret env vars come from the secrets manager, not committed files.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
)

const (
	envSuperAdminEmail = "LEADKART_SUPERADMIN__EMAIL"
	// gosec G101 false positive: this is the NAME of an env var read
	// at runtime, not a hardcoded password value.
	envSuperAdminPassword  = "LEADKART_SUPERADMIN__PASSWORD" //nolint:gosec // G101: env var name, not a credential
	envSuperAdminFirstName = "LEADKART_SUPERADMIN__FIRST_NAME"
	envSuperAdminLastName  = "LEADKART_SUPERADMIN__LAST_NAME"

	platformTenantSlug = "platform"
	platformLegalName  = "LeadKart Platform"
	platformDispName   = "Platform"

	superAdminRoleName       = "SuperAdmin"
	superAdminHierarchyLevel = 0
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	statusOnly := flag.Bool("status", false, "report current bootstrap state without writing")
	flag.Parse()

	dsn := os.Getenv("LEADKART_POSTGRES__DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return errors.New("LEADKART_POSTGRES__DSN env var required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}

	if *statusOnly {
		return reportStatus(ctx, db, logger)
	}

	email := strings.TrimSpace(os.Getenv(envSuperAdminEmail))
	password := os.Getenv(envSuperAdminPassword)
	if email == "" || password == "" {
		logger.WarnContext(ctx, "bootstrap skipped: SuperAdmin env vars not set",
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

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := seedSuperAdmin(ctx, tx, email, password, firstName, lastName, logger); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// seedSuperAdmin runs the five idempotent INSERTs in the order
// required by the FK graph: tenant → person → role → membership →
// role_assignment.
func seedSuperAdmin(ctx context.Context, tx *sql.Tx, email, password, firstName, lastName string, logger *slog.Logger) error {
	now := time.Now().UTC()

	// 1. Platform tenant. admin_email column was dropped in migration
	// 20260507000008 — current admin email is derived at read time
	// via JOIN through CompanyOwner role → person.email.
	tenantID := ids.NewV7().String()
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO identity.tenants (
		    id, slug, legal_name, display_name, status,
		    created_at, activated_at
		) VALUES ($1, $2, $3, $4, 'active', $5, $5)
		ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id
	`, tenantID, platformTenantSlug, platformLegalName, platformDispName, now).
		Scan(&tenantID); err != nil {
		return fmt.Errorf("upsert platform tenant: %w", err)
	}

	// 2. SuperAdmin Person.
	hash, err := argon2.Hash(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
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
		return fmt.Errorf("upsert person: %w", err)
	}

	// 3. SuperAdmin Role under platform tenant.
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
		return fmt.Errorf("upsert role: %w", err)
	}

	// 4. Membership.
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
		return fmt.Errorf("upsert membership: %w", err)
	}

	// 5. Role assignment.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity.role_assignments (
		    membership_id, role_id, tenant_id, assigned_at
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (membership_id, role_id) DO NOTHING
	`, membershipID, roleID, tenantID, now); err != nil {
		return fmt.Errorf("upsert role assignment: %w", err)
	}

	logger.InfoContext(ctx, "SuperAdmin bootstrapped",
		"tenant_id", tenantID,
		"person_id", personID,
		"membership_id", membershipID,
		"role_id", roleID,
		"email", email,
	)
	return nil
}

// reportStatus runs read-only queries to confirm whether the
// platform tenant + SuperAdmin role + Person all exist. Useful for
// CI dry-runs + ops triage.
func reportStatus(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	var (
		tenantID   sql.NullString
		roleID     sql.NullString
		personID   sql.NullString
		hasMember  bool
		hasAssign  bool
	)

	if err := db.QueryRowContext(ctx, `
		SELECT id FROM identity.tenants WHERE slug = $1
	`, platformTenantSlug).Scan(&tenantID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("status: tenant lookup: %w", err)
	}
	if tenantID.Valid {
		_ = db.QueryRowContext(ctx, `
			SELECT id FROM identity.roles
			WHERE  tenant_id = $1 AND name = $2 AND NOT is_deleted
		`, tenantID.String, superAdminRoleName).Scan(&roleID)
	}
	email := strings.TrimSpace(os.Getenv(envSuperAdminEmail))
	if email != "" {
		_ = db.QueryRowContext(ctx, `
			SELECT id FROM identity.persons WHERE email = $1
		`, email).Scan(&personID)
	}
	if tenantID.Valid && personID.Valid {
		_ = db.QueryRowContext(ctx, `
			SELECT EXISTS(
			    SELECT 1 FROM identity.tenant_memberships
			    WHERE person_id = $1 AND tenant_id = $2
			)
		`, personID.String, tenantID.String).Scan(&hasMember)
	}
	if hasMember && roleID.Valid {
		_ = db.QueryRowContext(ctx, `
			SELECT EXISTS(
			    SELECT 1 FROM identity.role_assignments ra
			    JOIN   identity.tenant_memberships m ON m.id = ra.membership_id
			    WHERE  m.person_id = $1 AND ra.role_id = $2
			)
		`, personID.String, roleID.String).Scan(&hasAssign)
	}

	logger.InfoContext(ctx, "bootstrap status",
		"platform_tenant_exists", tenantID.Valid,
		"superadmin_role_exists", roleID.Valid,
		"superadmin_person_exists", personID.Valid,
		"membership_exists", hasMember,
		"assignment_exists", hasAssign,
		"fully_bootstrapped", tenantID.Valid && roleID.Valid && personID.Valid && hasMember && hasAssign,
	)
	return nil
}
