//go:build integration

package pg_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
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

	if err := pg.EnsureGooseDialect(); err != nil {
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
// Call it BEFORE applyMigrations: migration 20260606000001 grants the role its
// privileges, so there are deliberately no GRANT statements here.
//
// The test uses `SET ROLE leadkart_app` inside the connection rather than
// reconnecting, which is in-session and applies immediately.
func createAppRole(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `CREATE ROLE leadkart_app NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`); err != nil {
		t.Fatalf("setup leadkart_app: %v", err)
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
		// identity.outbox retired by ADR 0064/0067 — replaced by the shared
		// common.outbox relay (migration 20260604000002).
		"processed_messages",                                           // 20260507000001
		"roles", "role_assignments", "membership_permission_overrides", // 20260507000002
		"permission_requests",    // 20260523000003 (Wave 9.1e / ADR 0055)
		"role_hierarchy_edges",   // 20260523000007 (Wave 9.4 / ADR 0058)
		"login_phantom_failures", // 20261120000004 (failed-login counter of unusable addresses / ADR 0068 D12)
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

	// Single source of truth for the `common.*` cross-cutting tables
	// (ADR 0067 rename of buildingblocks → common; command_idempotency
	// folded in from app). Same enumerated-list discipline as
	// expectedIdentityTables above — the count assertion derives from
	// len() so the test fails-loud on an extra OR a missing table.
	expectedCommonTables := []string{
		"command_idempotency",       // 20260507000001 (moved from app per ADR 0067)
		"audit_log_entry",           // 20260507000001
		"admin_impersonation_audit", // 20260507000006 (ADR 0045)
		"dead_letter",               // 20260604000001 (ADR 0067)
		"outbox",                    // 20260604000002 (ADR 0064 shared relay)
	}
	for _, table := range expectedCommonTables {
		var exists bool
		if err := db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM pg_tables
				WHERE schemaname = 'common' AND tablename = $1)
		`, table).Scan(&exists); err != nil {
			t.Fatalf("check common.%s: %v", table, err)
		}
		if !exists {
			t.Fatalf("common.%s missing after migration", table)
		}
	}
	var commonCount, appCount int
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_tables WHERE schemaname = 'common'
	`).Scan(&commonCount); err != nil {
		t.Fatalf("count common tables: %v", err)
	}
	if want := len(expectedCommonTables); commonCount != want {
		t.Fatalf("common tables: got %d, want %d", commonCount, want)
	}
	// app holds only the RLS GUC functions now — zero tables.
	if err := db.QueryRow(`
		SELECT count(*) FROM pg_tables WHERE schemaname = 'app'
	`).Scan(&appCount); err != nil {
		t.Fatalf("count app tables: %v", err)
	}
	if appCount != 0 {
		t.Fatalf("app tables: got %d, want 0 (command_idempotency moved to common; app is RLS-functions only)", appCount)
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

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	createAppRole(t, db)
	applyMigrations(t, dsn)

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
		mid uuid.UUID
		pid uuid.UUID
		tid uuid.UUID
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
	_ = tx.Rollback() // arch-test:ignore-err — cleanup; tx scope ends here

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
	_ = tx.Rollback() // arch-test:ignore-err — cleanup; tx scope ends here

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
	_ = tx.Rollback() // arch-test:ignore-err — cleanup; tx scope ends here
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

// ----- Application role privileges (migration 20260606000001) ----------------

// appSchemas returns every schema the migrations created (all but pg_*,
// information_schema and public): a future module schema shows up here
// automatically, which is what makes the privilege tests below fail CI when its
// init migration forgets the grants.
func appSchemas(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, db, `
		SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname NOT IN ('information_schema', 'public')
		ORDER BY nspname`)
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), q, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// pgArray renders a text[] literal for ANY($1) without importing pgtype.
func pgArray(items []string) string { return "{" + strings.Join(items, ",") + "}" }

func hasPriv(t *testing.T, db *sql.DB, q string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRowContext(t.Context(), q, args...).Scan(&ok); err != nil {
		t.Fatalf("%s %v: %v", q, args, err)
	}
	return ok
}

// leadkart_app, created before the migrations run (as docker/postgres/init.sql
// does in production), ends with exactly the privileges the application needs
// on every table, sequence and function of every application schema, and with
// nothing more.
func TestAppRoleHasPrivilegesOnEveryApplicationTable(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	schemas := appSchemas(t, db)
	if len(schemas) < 10 {
		t.Fatalf("expected the application schemas, found %v", schemas)
	}

	var super, bypass, createDB, createRole bool
	if err := db.QueryRowContext(t.Context(),
		`SELECT rolsuper, rolbypassrls, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = 'leadkart_app'`).
		Scan(&super, &bypass, &createDB, &createRole); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if super || bypass || createDB || createRole {
		t.Fatalf("leadkart_app must be a plain role: super=%v bypassrls=%v createdb=%v createrole=%v", super, bypass, createDB, createRole)
	}
	if owned := queryStrings(t, db, `
		SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relowner = (SELECT oid FROM pg_roles WHERE rolname = 'leadkart_app')`); len(owned) != 0 {
		t.Fatalf("leadkart_app must own nothing, owns %v", owned)
	}

	for _, s := range schemas {
		usage := hasPriv(t, db, `SELECT has_schema_privilege('leadkart_app', $1, 'USAGE')`, s)
		create := hasPriv(t, db, `SELECT has_schema_privilege('leadkart_app', $1, 'CREATE')`, s)
		if !usage || create {
			t.Errorf("schema %s: USAGE=%v CREATE=%v, want USAGE only: a new schema needs the grants and default privileges of migration 20260606000001 in its own init migration", s, usage, create)
		}
	}

	// Every table: exactly the expected privileges, checked in both directions
	// (each of the seven table privileges is either expected or must be absent).
	// Default = DML; the pinned exceptions below are the only narrower sets.
	// The Go side (pg.TablePrivilegeExceptions / SchemaTablePrivileges, read by
	// the start-up check) must say the same.
	dml := []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	wantSchemaPrivs := map[string][]string{"shared": {"SELECT"}}
	wantTableExceptions := map[string][]string{
		"common.audit_log_entry":           {"SELECT", "INSERT", "DELETE"},
		"common.admin_impersonation_audit": {"SELECT", "INSERT"},
	}
	if diff := cmp.Diff(wantSchemaPrivs, pg.SchemaTablePrivileges()); diff != "" {
		t.Errorf("pg.SchemaTablePrivileges differs from the pinned set (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantTableExceptions, pg.TablePrivilegeExceptions()); diff != "" {
		t.Errorf("pg.TablePrivilegeExceptions differs from the pinned set (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(dml, pg.DefaultTablePrivileges()); diff != "" {
		t.Errorf("pg.DefaultTablePrivileges differs (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(schemas, slices.Sorted(slices.Values(pg.ApplicationSchemas()))); diff != "" {
		t.Errorf("pg.ApplicationSchemas differs from the schemas the migrations created (-database +go):\n%s", diff)
	}
	tables := queryStrings(t, db, `
		SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1) ORDER BY 1`, pgArray(schemas))
	if len(tables) < 40 {
		t.Fatalf("expected the application tables, found %d", len(tables))
	}
	for tb := range wantTableExceptions {
		if !slices.Contains(tables, tb) {
			t.Errorf("pinned exception table %s does not exist", tb)
		}
	}
	for _, tb := range tables {
		want := dml
		if p, ok := wantTableExceptions[tb]; ok {
			want = p
		} else if p, ok := wantSchemaPrivs[strings.SplitN(tb, ".", 2)[0]]; ok {
			want = p
		}
		for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			got := hasPriv(t, db, `SELECT has_table_privilege('leadkart_app', $1, $2)`, tb, p)
			if expected := slices.Contains(want, p); got != expected {
				t.Errorf("leadkart_app %s on %s: held=%v, want %v (a schema missing the grants of migration 20260606000001, or a privilege that should have been removed)", p, tb, got, expected)
			}
		}
	}

	// Every sequence: USAGE + SELECT, not UPDATE (the read-only `shared` schema has none).
	for _, sq := range queryStrings(t, db, `
		SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'S' AND n.nspname = ANY($1) ORDER BY 1`, pgArray(schemas)) {
		if strings.HasPrefix(sq, "shared.") {
			t.Errorf("sequence %s in the read-only shared schema: decide its privileges explicitly", sq)
			continue
		}
		usage := hasPriv(t, db, `SELECT has_sequence_privilege('leadkart_app', $1, 'USAGE')`, sq)
		sel := hasPriv(t, db, `SELECT has_sequence_privilege('leadkart_app', $1, 'SELECT')`, sq)
		upd := hasPriv(t, db, `SELECT has_sequence_privilege('leadkart_app', $1, 'UPDATE')`, sq)
		if !usage || !sel || upd {
			t.Errorf("sequence %s: USAGE=%v SELECT=%v UPDATE=%v, want USAGE+SELECT only", sq, usage, sel, upd)
		}
	}

	// The RLS helper functions the policies call.
	for _, fn := range []string{"app.current_tenant()", "app.is_platform()"} {
		if !hasPriv(t, db, `SELECT has_function_privilege('leadkart_app', $1, 'EXECUTE')`, fn) {
			t.Errorf("leadkart_app cannot EXECUTE %s", fn)
		}
	}

	// goose keeps its ledger out of the application's reach.
	if hasPriv(t, db, `SELECT has_table_privilege('leadkart_app', 'public.goose_db_version', 'SELECT')`) {
		t.Error("leadkart_app must have no privileges on goose's version table")
	}
}

// A real read against every RLS table, as leadkart_app (SET ROLE), proves the
// grants are usable (no "permission denied") while row-level security is in
// force for the role (migration-seeded config tables such as platform.lead_tiers
// are readable by design, so row counts are not asserted).
func TestAppRoleCanReadEveryApplicationTableUnderRLS(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	tables := queryStrings(t, db, `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1) AND c.relrowsecurity ORDER BY 1`, pgArray(appSchemas(t, db)))
	if len(tables) == 0 {
		t.Fatal("no RLS tables found")
	}
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `SET ROLE leadkart_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	for _, tb := range tables {
		var n int
		if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM `+tb).Scan(&n); err != nil {
			t.Errorf("read %s as leadkart_app: %v", tb, err)
		}
	}
}

// appRoleGrantsMigration is the version of 20260606000001_application_role_grants.sql.
const appRoleGrantsMigration = 20260606000001

// Down then up again leaves the same privileges (reversible, idempotent). With
// no leadkart_app role the migration is a no-op: TestMigrationsApplyCleanly
// runs the whole set without one.
func TestAppRoleGrantsMigration_DownThenUpIsIdempotent(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	usage := func() bool {
		return hasPriv(t, db, `SELECT has_schema_privilege('leadkart_app', 'identity', 'USAGE')`)
	}
	if !usage() {
		t.Fatal("grants missing after up")
	}
	gdb, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	defer gdb.Close()
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	// Roll back through the grants migration (and any migration added after it),
	// not just the newest one: later migrations must not change what this pins.
	if err := goose.DownToContext(t.Context(), gdb, migrationsDir(t), appRoleGrantsMigration-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if usage() {
		t.Fatal("grants survived the down migration")
	}
	if err := goose.UpContext(t.Context(), gdb, migrationsDir(t)); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
	if !usage() {
		t.Fatal("grants missing after re-up")
	}
}

// ----- RLS policies use the safe tenant helpers (migration 20261120000001) ---

// seedTwoTenantsWithPermissionRequests inserts two tenants, one membership each
// and one permission request each, as the owner (which bypasses RLS), and
// returns tenant A, tenant B and B's request id.
func seedTwoTenantsWithPermissionRequests(t *testing.T, db *sql.DB) (tenantA, tenantB, requestB uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	tenantA, tenantB, requestB = uuid.New(), uuid.New(), uuid.New()
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		person, membership, request := uuid.New(), uuid.New(), uuid.New()
		if tid == tenantB {
			request = requestB
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at)
			VALUES ($1, $2, 'Acme Pharma', 'Acme', 'active', now())`, tid, "tenant-"+tid.String()[:8]); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, created_at)
			VALUES ($1, $2, 'First', 'Last', 'argon2id$...', $3, now())`, person, person.String()+"@example.test", uuid.New()); err != nil {
			t.Fatalf("insert person: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at)
			VALUES ($1, $2, $3, 'active', now())`, membership, person, tid); err != nil {
			t.Fatalf("insert membership: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO identity.permission_requests
				(id, tenant_id, requester_membership_id, permission_constant, duration_days, reason, state)
			VALUES ($1, $2, $3, 'crm.leads.view', 7, 'need it for the quarter close', 'pending')`, request, tid, membership); err != nil {
			t.Fatalf("insert permission request: %v", err)
		}
	}
	return tenantA, tenantB, requestB
}

// On ONE reused connection as leadkart_app: a platform-scoped transaction ends,
// then a tenant-scoped read of another tenant's permission request by id must
// answer "no rows". After the first transaction `app.is_platform` reads an empty string on
// the pooled connection, and a raw `current_setting(...)::boolean` in a policy
// raises 22P02 (a 500 plus an existence signal for foreign ids).
func TestRLS_PermissionRequestsTenantReadAfterPlatformTxOnSameConnection(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)
	tenantA, _, requestB := seedTwoTenantsWithPermissionRequests(t, db)

	ctx := t.Context()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET ROLE leadkart_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// Platform-scoped transaction (the setting is transaction-local).
	ptx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin platform tx: %v", err)
	}
	if _, err := ptx.ExecContext(ctx, `SELECT set_config('app.is_platform','true',true)`); err != nil {
		t.Fatalf("set is_platform: %v", err)
	}
	var all int
	if err := ptx.QueryRowContext(ctx, `SELECT count(*) FROM identity.permission_requests`).Scan(&all); err != nil || all != 2 {
		t.Fatalf("platform scope: count=%d err=%v, want 2", all, err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit platform tx: %v", err)
	}

	// Tenant-scoped transaction on the SAME connection.
	ttx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer func() { _ = ttx.Rollback() }() // arch-test:ignore-err — cleanup; tx scope ends here
	if _, err := ttx.ExecContext(ctx, `SELECT set_config('app.tenant_id',$1,true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant_id: %v", err)
	}
	var got int
	if err := ttx.QueryRowContext(ctx, `SELECT count(*) FROM identity.permission_requests WHERE id = $1`, requestB).Scan(&got); err != nil {
		t.Fatalf("tenant A read of tenant B's request by id errored (want no rows): %v", err)
	}
	if got != 0 {
		t.Fatalf("tenant A saw %d rows of tenant B's request, want 0", got)
	}
}

// No policy anywhere may call current_setting() itself: an unset or reset
// setting reads an empty string and the cast raises. Policies go through app.current_tenant()
// / app.is_platform() (NULLIF + COALESCE). The sweep keeps the class from
// returning with a new table.
func TestRLS_NoPolicyUsesRawCurrentSetting(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(t.Context(), `
		SELECT schemaname || '.' || tablename || ' ' || policyname
		FROM pg_policies
		WHERE coalesce(qual, '') ILIKE '%current\_setting(%' OR coalesce(with_check, '') ILIKE '%current\_setting(%'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("query pg_policies: %v", err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		bad = append(bad, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("policies with a raw current_setting() (use app.current_tenant() / app.is_platform()): %v", bad)
	}
}

// ----- Start-up privilege check ----------------------------------------------

// appRoleConn opens a connection and switches it to leadkart_app, so
// current_user (what the check inspects) is the application role.
func appRoleConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) // arch-test:context-background — t.Context() is cancelled when Cleanup runs
	if _, err := conn.Exec(t.Context(), `SET ROLE leadkart_app`); err != nil {
		t.Fatalf("set role: %v", err)
	}
	return conn
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestVerifyApplicationPrivileges_PassesOnMigratedDatabase(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	if err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()}); err != nil {
		t.Fatalf("a correctly migrated database must pass (including the narrower audit and shared sets): %v", err)
	}
}

// The grants migration records itself as applied without granting anything when
// the role does not exist yet. Creating the role afterwards leaves it with
// nothing, and the check must say so (and not leak the DSN).
func TestVerifyApplicationPrivileges_FailsWhenRoleWasCreatedAfterMigrating(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn) // no leadkart_app yet: the grants migration is a no-op
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)

	err = pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()})
	if err == nil {
		t.Fatal("check passed for a role that holds nothing")
	}
	for _, want := range []string{`"leadkart_app"`, "USAGE on schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "leadkart_test") || strings.Contains(err.Error(), "postgres://") {
		t.Errorf("error leaks connection details: %v", err)
	}
}

// A single missing table privilege is named, and the append-only exceptions are
// enforced in the direction the check can see (DELETE on the audit log is needed).
func TestVerifyApplicationPrivileges_NamesTheFirstMissingPrivilege(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)
	if _, err := db.ExecContext(t.Context(), `REVOKE DELETE ON common.audit_log_entry FROM leadkart_app`); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	err = pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()})
	if err == nil || !strings.Contains(err.Error(), "DELETE on table common.audit_log_entry") {
		t.Fatalf("want an error naming DELETE on common.audit_log_entry, got: %v", err)
	}
}

// An UNMIGRATED database yields no rows in any catalog check, so it used to
// pass. The check now asserts the schemas and RLS helpers exist and points at
// `migrate up`.
func TestVerifyApplicationPrivileges_FailsOnUnmigratedDatabase(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)

	err = pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Logger: discardLogger()})
	if err == nil {
		t.Fatal("an unmigrated database passed the start-up check")
	}
	for _, want := range []string{"not migrated", "does not exist", "migrate up"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// A role with too much privilege silently switches row-level security off.
// Production refuses to start; every other environment logs a warning and runs.
func TestVerifyApplicationPrivileges_RefusesAnOverPrivilegedRoleInProduction(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	for _, tc := range []struct{ name, grant, revert, want string }{
		{"superuser", `ALTER ROLE leadkart_app SUPERUSER`, `ALTER ROLE leadkart_app NOSUPERUSER`, "is a superuser"},
		{"bypassrls", `ALTER ROLE leadkart_app BYPASSRLS`, `ALTER ROLE leadkart_app NOBYPASSRLS`, "BYPASSRLS"},
		{"owner of an application table", `ALTER TABLE identity.persons OWNER TO leadkart_app`,
			`ALTER TABLE identity.persons OWNER TO leadkart; GRANT SELECT, INSERT, UPDATE, DELETE ON identity.persons TO leadkart_app`, "owns table identity.persons"},
		{"member of a bypassrls role", `CREATE ROLE rls_bypasser BYPASSRLS; GRANT rls_bypasser TO leadkart_app`,
			`REVOKE rls_bypasser FROM leadkart_app; DROP ROLE rls_bypasser`, "member of the BYPASSRLS role rls_bypasser"},
	} {
		t.Run(tc.name, func(t *testing.T) { // arch-test:serial — each case mutates and restores the one shared role
			exec(tc.grant)
			defer exec(tc.revert)

			err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "production") {
				t.Fatalf("production: want a refusal containing %q, got %v", tc.want, err)
			}

			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			if err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Logger: logger}); err != nil {
				t.Fatalf("outside production the check must only warn, got %v", err)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), tc.want) {
				t.Errorf("want a WARN naming %q, got %q", tc.want, logs.String())
			}
			if strings.Contains(logs.String(), dsn) || strings.Contains(logs.String(), "leadkart_test") {
				t.Errorf("warning leaks connection details: %q", logs.String())
			}
		})
	}
}

// River's tables are part of the contract: a role that lost DML on river_job
// fails the check at boot, and the ledger table (owner-only) is not required.
func TestVerifyApplicationPrivileges_CoversRiverTables(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)
	for _, q := range []string{
		`CREATE TABLE public.river_job (id bigserial PRIMARY KEY)`,
		`CREATE TABLE public.river_migration (line text, version bigint)`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON public.river_job TO leadkart_app`,
		`GRANT USAGE, SELECT ON SEQUENCE public.river_job_id_seq TO leadkart_app`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()}); err != nil {
		t.Fatalf("river tables granted (ledger not required): %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `REVOKE DELETE ON public.river_job FROM leadkart_app`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err = pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()})
	if err == nil || !strings.Contains(err.Error(), "DELETE on table public.river_job") {
		t.Fatalf("want an error naming DELETE on public.river_job, got %v", err)
	}
}

// phantomFailuresMigration is the version of 20261120000004_identity_login_phantom_failures.sql.
const phantomFailuresMigration = 20261120000004

// The phantom failed-login table is created with DML for the application role,
// dropped by Down and recreated by Up again, and the start-up privilege check
// passes with it present.
func TestLoginPhantomFailuresMigration_UpDownUpAndGrants(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	exists := func() bool {
		return hasPriv(t, db, `SELECT to_regclass('identity.login_phantom_failures') IS NOT NULL`)
	}
	dml := func() bool {
		return hasPriv(t, db, `SELECT bool_and(has_table_privilege('leadkart_app', 'identity.login_phantom_failures', p))
			FROM unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE']) AS p`)
	}
	if !exists() || !dml() {
		t.Fatalf("after up: table=%v dml=%v, want both", exists(), dml())
	}
	if hasPriv(t, db, `SELECT has_table_privilege('leadkart_app', 'identity.login_phantom_failures', 'TRUNCATE')`) {
		t.Fatal("the application role must not hold TRUNCATE on the counter")
	}
	if err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()}); err != nil {
		t.Fatalf("start-up privilege check with the new table: %v", err)
	}

	gdb, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	defer gdb.Close()
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.DownToContext(t.Context(), gdb, migrationsDir(t), phantomFailuresMigration-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if exists() {
		t.Fatal("the table survived the down migration")
	}
	if err := goose.UpContext(t.Context(), gdb, migrationsDir(t)); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
	if !exists() || !dml() {
		t.Fatalf("after re-up: table=%v dml=%v, want both", exists(), dml())
	}
}

// resetAttemptsMigration is the version of 20261120000005_persons_password_reset_failed_attempts.sql.
const resetAttemptsMigration = 20261120000005

// The refused-reset-attempt counter is a NOT NULL column defaulting to 0 that
// the application role can read and write through its table grant, is dropped by
// Down and recreated by Up, and the start-up privilege check passes with it.
func TestPersonsResetAttemptsMigration_UpDownUpAndGrants(t *testing.T) {
	dsn := startPostgres(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	createAppRole(t, db)
	applyMigrations(t, dsn)

	column := func() bool {
		return hasPriv(t, db, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'identity' AND table_name = 'persons' AND column_name = 'password_reset_failed_attempts'
			  AND is_nullable = 'NO' AND column_default = '0')`)
	}
	write := func() bool {
		return hasPriv(t, db, `SELECT has_column_privilege('leadkart_app', 'identity.persons', 'password_reset_failed_attempts', 'UPDATE')
			AND has_column_privilege('leadkart_app', 'identity.persons', 'password_reset_failed_attempts', 'SELECT')`)
	}
	if !column() || !write() {
		t.Fatalf("after up: column(not null, default 0)=%v app read+write=%v, want both", column(), write())
	}
	if err := pg.VerifyApplicationPrivileges(t.Context(), appRoleConn(t, dsn), pg.PrivilegeCheck{Production: true, Logger: discardLogger()}); err != nil {
		t.Fatalf("start-up privilege check with the new column: %v", err)
	}

	gdb, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	defer gdb.Close()
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	if err := goose.DownToContext(t.Context(), gdb, migrationsDir(t), resetAttemptsMigration-1); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if column() {
		t.Fatal("the column survived the down migration")
	}
	if err := goose.UpContext(t.Context(), gdb, migrationsDir(t)); err != nil {
		t.Fatalf("goose up again: %v", err)
	}
	if !column() || !write() {
		t.Fatalf("after re-up: column=%v app read+write=%v, want both", column(), write())
	}
}
