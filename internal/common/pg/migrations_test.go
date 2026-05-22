//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// migrationsDir resolves to <repo>/migrations regardless of where `go test`
// is invoked from (cwd is the package dir under `go test ./...`).
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../internal/common/pg/migrations_test.go
	// repo = ../../../
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}

// startPostgres spins an ephemeral Postgres 17 via testcontainers and
// returns its DSN. Container is auto-cleaned via t.Cleanup.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	c, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("leadkart_test"),
		postgres.WithUsername("leadkart"),
		postgres.WithPassword("leadkart_test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

// applyMigrations runs `goose up` against dsn using the repo's migrations dir.
func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := goose.UpContext(ctx, db, migrationsDir(t)); err != nil {
		t.Fatalf("goose up: %v", err)
	}
}

// createAppRole creates a non-superuser role mirroring the production
// `leadkart_app` role (per multi-tenancy.md "Three Postgres roles") so that
// RLS actually fires — testcontainers' default user is a superuser, which
// unconditionally bypasses RLS and would mask isolation bugs.
//
// The test uses `SET ROLE leadkart_app` inside the connection rather than
// reconnecting, which is in-session and applies immediately.
func createAppRole(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	stmts := []string{
		`CREATE ROLE leadkart_app NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup leadkart_app: %s: %v", s, err)
		}
	}
}

// TestMigrationsApplyCleanly verifies the migration set lands without error
// against a fresh Postgres 17 instance — the floor for everything else.
func TestMigrationsApplyCleanly(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Single source of truth for the expected `identity.*` tables.
	// Migrations that add a table extend this list — the count
	// assertion derives from len(expectedIdentityTables) so the test
	// fails-loud on either an extra table OR a missing one.
	expectedIdentityTables := []string{
		"tenants", "persons", "tenant_memberships",
		"refresh_token_families", "refresh_tokens", "auth_routing",
		"outbox",
		"processed_messages",                                 // 20260507000001
		"roles", "role_assignments", "membership_permission_overrides", // 20260507000002
	}
	var count int
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_tables WHERE schemaname = 'identity'
	`).Scan(&count); err != nil {
		t.Fatalf("count identity tables: %v", err)
	}
	if want := len(expectedIdentityTables); count != want {
		t.Fatalf("identity tables: got %d, want %d", count, want)
	}
	for _, table := range expectedIdentityTables {
		var exists bool
		if err := db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM pg_tables
				WHERE schemaname = 'identity' AND tablename = $1)
		`, table).Scan(&exists); err != nil {
			t.Fatalf("check identity.%s: %v", table, err)
		}
		if !exists {
			t.Fatalf("identity.%s missing after migration", table)
		}
	}

	// Verify tenant_memberships gained the four profile/hierarchy columns.
	for _, col := range []string{"designation", "department", "status_message", "reports_to"} {
		var exists bool
		if err := db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'identity'
				  AND table_name   = 'tenant_memberships'
				  AND column_name  = $1)
		`, col).Scan(&exists); err != nil {
			t.Fatalf("check tenant_memberships.%s: %v", col, err)
		}
		if !exists {
			t.Fatalf("tenant_memberships.%s missing after migration", col)
		}
	}

	// Verify RLS+FORCE enabled on all three new authorization tables.
	for _, table := range []string{"roles", "role_assignments", "membership_permission_overrides"} {
		var rlsEnabled, rlsForced bool
		if err := db.QueryRow(`
			SELECT relrowsecurity, relforcerowsecurity
			FROM   pg_class c
			JOIN   pg_namespace n ON n.oid = c.relnamespace
			WHERE  n.nspname = 'identity' AND c.relname = $1
		`, table).Scan(&rlsEnabled, &rlsForced); err != nil {
			t.Fatalf("check RLS on identity.%s: %v", table, err)
		}
		if !rlsEnabled {
			t.Fatalf("identity.%s RLS not enabled", table)
		}
		if !rlsForced {
			t.Fatalf("identity.%s RLS not FORCEd (would let table-owner bypass)", table)
		}
	}

	// buildingblocks schema present + 2 tables: audit_log_entry (from
	// 20260507000001_messaging_infra) + admin_impersonation_audit
	// (from 20260507000006_admin_impersonation_audit, A.7.b
	// impersonation lifecycle).
	var bbCount, appCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_tables WHERE schemaname = 'buildingblocks'
	`).Scan(&bbCount); err != nil {
		t.Fatalf("count buildingblocks tables: %v", err)
	}
	if bbCount != 2 {
		t.Fatalf("buildingblocks tables: got %d want 2", bbCount)
	}
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_tables WHERE schemaname = 'app'
	`).Scan(&appCount); err != nil {
		t.Fatalf("count app tables: %v", err)
	}
	if appCount != 1 {
		t.Fatalf("app tables: got %d want 1 (command_idempotency)", appCount)
	}

	// app schema functions present.
	for _, fn := range []string{"current_tenant", "is_platform"} {
		var exists bool
		if err := db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM pg_proc p
				JOIN pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = 'app' AND p.proname = $1)
		`, fn).Scan(&exists); err != nil {
			t.Fatalf("check fn %s: %v", fn, err)
		}
		if !exists {
			t.Fatalf("app.%s() missing", fn)
		}
	}
}

// TestRLSIsolatesTenants is the load-bearing test for the entire
// multi-tenancy model: with `app.tenant_id = X`, SELECT against
// identity.tenant_memberships returns ONLY tenant X's rows.
func TestRLSIsolatesTenants(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	createAppRole(t, db)

	ctx := t.Context()
	tenantA, tenantB := uuid.New(), uuid.New()
	personA, personB := uuid.New(), uuid.New()

	// Seed: two tenants + two persons + one membership each.
	// Memberships are the RLS-scoped table — seeding requires platform GUC.
	seedTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := seedTx.ExecContext(ctx, `SELECT set_config('app.is_platform','true',true)`); err != nil {
		t.Fatalf("seed set is_platform: %v", err)
	}
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		if _, err := seedTx.ExecContext(ctx, `
			INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at)
			VALUES ($1, $2, 'Acme Pharma', 'Acme', 'active', now())
		`, tid, "tenant-"+tid.String()[:8]); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}
	for _, p := range []struct {
		id    uuid.UUID
		email string
	}{{personA, "alice@example.test"}, {personB, "bob@example.test"}} {
		if _, err := seedTx.ExecContext(ctx, `
			INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, created_at)
			VALUES ($1, $2, 'First', 'Last', 'argon2id$...', $3, now())
		`, p.id, p.email, uuid.New()); err != nil {
			t.Fatalf("insert person: %v", err)
		}
	}
	for _, m := range []struct {
		mid   uuid.UUID
		pid   uuid.UUID
		tid   uuid.UUID
	}{{uuid.New(), personA, tenantA}, {uuid.New(), personB, tenantB}} {
		if _, err := seedTx.ExecContext(ctx, `
			INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at)
			VALUES ($1, $2, $3, 'active', now())
		`, m.mid, m.pid, m.tid); err != nil {
			t.Fatalf("insert membership: %v", err)
		}
	}
	if err := seedTx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// As tenantA (under leadkart_app role): see exactly 1 row.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tenantA tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE leadkart_app`); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant_id: %v", err)
	}
	var seen int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity.tenant_memberships`).Scan(&seen); err != nil {
		t.Fatalf("count as tenantA: %v", err)
	}
	if seen != 1 {
		t.Fatalf("tenantA scope: saw %d memberships, want 1", seen)
	}

	// Verify it's tenantA's row, not tenantB's.
	var seenTenant uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM identity.tenant_memberships`).Scan(&seenTenant); err != nil {
		t.Fatalf("read tenant_id as tenantA: %v", err)
	}
	if seenTenant != tenantA {
		t.Fatalf("tenantA saw tenant_id %s, want %s", seenTenant, tenantA)
	}
	_ = tx.Rollback()

	// Platform context (under leadkart_app role): see both rows.
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin platform tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE leadkart_app`); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.is_platform','true',true)`); err != nil {
		t.Fatalf("set is_platform: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity.tenant_memberships`).Scan(&seen); err != nil {
		t.Fatalf("count as platform: %v", err)
	}
	if seen != 2 {
		t.Fatalf("platform scope: saw %d memberships, want 2", seen)
	}
	_ = tx.Rollback()

	// No GUC set, leadkart_app role: zero rows (fail-closed).
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin neutral tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE leadkart_app`); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM identity.tenant_memberships`).Scan(&seen); err != nil {
		t.Fatalf("count neutral: %v", err)
	}
	if seen != 0 {
		t.Fatalf("neutral scope: saw %d memberships, want 0 (fail-closed)", seen)
	}
	_ = tx.Rollback()
}

// TestSingleActiveMembershipInvariant verifies the partial unique index
// blocks a Person from holding two concurrent Active Memberships across
// tenants — the database-level enforcement of the login-flow invariant.
func TestSingleActiveMembershipInvariant(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := t.Context()
	tenantA, tenantB := uuid.New(), uuid.New()
	person := uuid.New()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.is_platform','true',true)`); err != nil {
		t.Fatalf("set platform: %v", err)
	}
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at)
			VALUES ($1, $2, 'X', 'X', 'active', now())
		`, tid, "t-"+tid.String()[:8]); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, created_at)
		VALUES ($1, 'p@x.test', 'P', 'X', 'h', $2, now())
	`, person, uuid.New()); err != nil {
		t.Fatalf("insert person: %v", err)
	}

	// First Active Membership — accepted.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at)
		VALUES ($1, $2, $3, 'active', now())
	`, uuid.New(), person, tenantA); err != nil {
		t.Fatalf("first active membership rejected: %v", err)
	}

	// Second concurrent Active Membership — must violate the partial
	// unique index uq_memberships_person_active.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at)
		VALUES ($1, $2, $3, 'active', now())
	`, uuid.New(), person, tenantB)
	if err == nil {
		t.Fatal("second active membership accepted: invariant broken")
	}
	// Postgres SQLSTATE 23505 = unique_violation.
	got := err.Error()
	if !strings.Contains(got, "23505") && !strings.Contains(got, "uq_memberships_person_active") {
		t.Fatalf("expected unique violation on uq_memberships_person_active, got: %v", err)
	}
}
