//go:build integration

package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
)

// TestSeedSuperAdmin_FreshDatabase exercises the full bootstrap path
// against a real testcontainers Postgres + the canonical migration set.
// Asserts:
//   - All 5 rows land (tenant, person, role, membership, role_assignment)
//   - Tenant slug = "platform", role.is_super_admin = true
//   - Argon2id hash round-trips against the supplied plaintext
//   - Re-running is a free no-op (ON CONFLICT branches do their job)
func TestSeedSuperAdmin_FreshDatabase(t *testing.T) {
	const (
		email     = "platform-admin@bootstrap.test"
		password  = "BootstrapTestPassword!2026"
		firstName = "Boot"
		lastName  = "Strap"
	)

	ctx := context.Background()
	db := startTestDB(t, ctx)

	// 1. First run — seeds 5 rows.
	if err := runOnce(ctx, db, email, password, firstName, lastName, true); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	assertSeedState(t, ctx, db, email, password, firstName, lastName)

	// 2. Re-run on already-seeded DB — must be a no-op (no errors,
	//    no duplicate rows). ON CONFLICT branches do the heavy lifting.
	if err := runOnce(ctx, db, email, password, firstName, lastName, true); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	assertSeedState(t, ctx, db, email, password, firstName, lastName)
	assertRowCounts(t, ctx, db, 1)
}

// TestSeedSuperAdmin_MissingEnv_NoOp asserts the env-skip contract
// against a real DB (paranoia: confirms no rows leak in when the
// caller hands us empty creds).
func TestSeedSuperAdmin_MissingEnv_NoOp(t *testing.T) {
	ctx := context.Background()
	db := startTestDB(t, ctx)

	if err := runOnce(ctx, db, "", "", "", "", false); err != nil {
		t.Fatalf("expected nil on empty env, got %v", err)
	}
	assertRowCounts(t, ctx, db, 0)
}

func assertSeedState(t *testing.T, ctx context.Context, db *sql.DB, email, password, firstName, lastName string) {
	t.Helper()

	// Tenant.
	var (
		tenantID     string
		tenantSlug   string
		tenantStatus string
		legalName    string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, slug, status, legal_name
		FROM   identity.tenants WHERE slug = $1
	`, "platform").Scan(&tenantID, &tenantSlug, &tenantStatus, &legalName); err != nil {
		t.Fatalf("query platform tenant: %v", err)
	}
	if tenantSlug != "platform" {
		t.Errorf("tenant slug = %q, want %q", tenantSlug, "platform")
	}
	if tenantStatus != "active" {
		t.Errorf("tenant status = %q, want active", tenantStatus)
	}
	if legalName != "LeadKart Platform" {
		t.Errorf("tenant legal_name = %q, want LeadKart Platform", legalName)
	}

	// Person — Argon2 hash round-trip.
	var (
		personID     string
		personEmail  string
		passwordHash string
		fname, lname string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, email, password_hash, first_name, last_name
		FROM   identity.persons WHERE email = $1
	`, email).Scan(&personID, &personEmail, &passwordHash, &fname, &lname); err != nil {
		t.Fatalf("query person: %v", err)
	}
	if fname != firstName || lname != lastName {
		t.Errorf("person name = %q %q, want %q %q", fname, lname, firstName, lastName)
	}
	// Verify the hashed password actually decodes against the plain.
	// Direct argon2 verify here so the test catches a future swap of
	// the hashing algorithm at the bootstrap site.
	if err := verifyPassword(t, passwordHash, password); err != nil {
		t.Errorf("password hash round-trip: %v", err)
	}

	// Role — under platform tenant, is_super_admin = true.
	var (
		roleID       string
		roleName     string
		isSuperAdmin bool
		isDefault    bool
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, name, is_super_admin, is_system_default
		FROM   identity.roles
		WHERE  tenant_id = $1 AND name = $2 AND NOT is_deleted
	`, tenantID, "SuperAdmin").Scan(&roleID, &roleName, &isSuperAdmin, &isDefault); err != nil {
		t.Fatalf("query role: %v", err)
	}
	if !isSuperAdmin {
		t.Errorf("role.is_super_admin = false, want true")
	}
	if !isDefault {
		t.Errorf("role.is_system_default = false, want true")
	}

	// Membership — Person ↔ platform tenant, status active.
	var (
		membershipID     string
		membershipStatus string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, status FROM identity.tenant_memberships
		WHERE  person_id = $1 AND tenant_id = $2
	`, personID, tenantID).Scan(&membershipID, &membershipStatus); err != nil {
		t.Fatalf("query membership: %v", err)
	}
	if membershipStatus != "active" {
		t.Errorf("membership status = %q, want active", membershipStatus)
	}

	// Role assignment — Membership ↔ Role.
	var assigned int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM identity.role_assignments
		WHERE  membership_id = $1 AND role_id = $2
	`, membershipID, roleID).Scan(&assigned); err != nil {
		t.Fatalf("query role_assignment: %v", err)
	}
	if assigned != 1 {
		t.Errorf("role_assignments count = %d, want 1", assigned)
	}
}

func assertRowCounts(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()

	var tenants, persons, members, assigns, roles int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM identity.tenants WHERE slug = 'platform'`).Scan(&tenants); err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM identity.tenant_memberships`).Scan(&members); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM identity.persons`).Scan(&persons); err != nil {
		t.Fatalf("count persons: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM identity.roles WHERE is_super_admin AND NOT is_deleted`).Scan(&roles); err != nil {
		t.Fatalf("count super-admin roles: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM identity.role_assignments`).Scan(&assigns); err != nil {
		t.Fatalf("count role_assignments: %v", err)
	}

	if tenants != want {
		t.Errorf("platform tenant rows = %d, want %d", tenants, want)
	}
	if persons != want {
		t.Errorf("person rows = %d, want %d", persons, want)
	}
	if members != want {
		t.Errorf("membership rows = %d, want %d", members, want)
	}
	if roles != want {
		t.Errorf("super-admin role rows = %d, want %d", roles, want)
	}
	if assigns != want {
		t.Errorf("role_assignment rows = %d, want %d", assigns, want)
	}
}

// startTestDB spins testcontainers Postgres + applies migrations +
// returns a *sql.DB connected as the owning superuser. Bootstrap
// runs as the superuser in production too (it bypasses RLS via
// owner privileges) — the test mirrors that.
func startTestDB(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	startCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	c, err := postgres.Run(startCtx,
		"postgres:17-alpine",
		postgres.WithDatabase("leadkart_bootstrap_test"),
		postgres.WithUsername("leadkart"),
		postgres.WithPassword("leadkart_test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	dsn, err := c.ConnectionString(startCtx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	gooseDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(startCtx, gooseDB, resolveBootstrapMigrationsDir(t)); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	_ = gooseDB.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(startCtx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

func resolveBootstrapMigrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../cmd/bootstrap/seed_integration_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "migrations")
}

// verifyPassword wraps argon2.Verify so the assertion site stays
// compact + the test reads top-down.
func verifyPassword(t *testing.T, hashed, plain string) error {
	t.Helper()
	return argon2.Verify(plain, hashed)
}
