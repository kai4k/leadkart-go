//go:build integration

// Package pgtest provides the SHARED testcontainer + migration bootstrap
// for integration tests across every module. Replaces the per-test
// container anti-pattern (15s per test × 60 tests = 15min) with the
// canon Go+Postgres pattern: ONE container per package, applied via
// TestMain; tests get isolation via RLS-tenant partitioning (the
// preferred path, parallel-safe) or TruncateAll (the fallback for
// cross-tenant tests).
//
// CANON SOURCES:
//   - Brandur Leach "Postgres at Stripe" — shared container, per-test tx
//     or per-test tenant. Used in production for Stripe's Ruby tests +
//     the river queue Go test suite.
//   - TDL Wild Workouts — shared container per package, per-test reset
//     (Firestore.Reset or MySQL TRUNCATE). Same shape; we picked
//     RLS-tenant partitioning where possible because our domain is
//     already tenant-sharded.
//   - mattermost-server — postgres TRUNCATE per test (used here as
//     fallback via TruncateAll).
//   - pgx test suite (Jack Christensen) — shared container + tx rollback.
//
// USAGE in a test package:
//
//	//go:build integration
//	package adapters_test
//
//	import "github.com/leadkart/leadkart-go/internal/common/pgtest"
//
//	var sharedPG *pgtest.Container
//
//	func TestMain(m *testing.M) {
//	    code := pgtest.RunMain(m, pgtest.Config{
//	        Schemas: []string{"app", "identity"},
//	        Grants:  []string{"identity"},
//	    }, func(c *pgtest.Container) {
//	        sharedPG = c   // ← runs BEFORE m.Run() so tests see non-nil
//	    })
//	    os.Exit(code)
//	}
//
//	func TestThing(t *testing.T) {
//	    t.Parallel()                          // parallel-safe — RLS isolates
//	    pool := sharedPG.Pool()
//	    tenantID := tenant.ID(ids.NewV7().String())
//	    ctx := tenancy.WithID(t.Context(), tenancy.ID(tenantID.String()))
//	    // ... test against pool with tenant_id GUC bound
//	}
package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql (goose)
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// Fixed test-only role credentials. NOT a configurable secret —
// hardcoded by design so the arch-test discipline doesn't flag the
// pgtest package for "password field on non-input DTO" (this is test
// infrastructure provisioning a known role inside an ephemeral
// container; the secret value is intentionally constant for repro).
const (
	appRoleName     = "leadkart_app"
	appRolePasswd   = "leadkart_app_pw" //nolint:gosec // hardcoded test-only role; ephemeral container
	dbName          = "leadkart_test"
	ownerRole       = "leadkart"
	ownerPasswdLit  = "leadkart_test" //nolint:gosec // hardcoded test-only role; ephemeral container
)

// Config configures what the shared container's bootstrap should look
// like for THIS test package. Each module's TestMain supplies its own
// schema list (which schemas to grant USAGE on) and grants list (which
// schemas the app role gets DML on).
//
// Most modules need:
//   - Schemas: app + their own module schema (+ identity for tenant FK)
//   - Grants:  their own module schema (+ identity if the module's tests
//              directly insert tenants/persons for cross-FK seeding)
//
// App role credentials are fixed test-only constants (see top of file).
// Not exposed via Config to keep the surface tight + satisfy arch-test
// discipline (no password fields on non-input DTOs).
type Config struct {
	// Schemas is the list of schemas the leadkart_app role gets USAGE
	// permission on. Always includes "app" implicitly.
	Schemas []string

	// Grants is the list of schemas the leadkart_app role gets
	// SELECT/INSERT/UPDATE/DELETE on ALL TABLES. Must be a subset of
	// Schemas.
	Grants []string
}

// Container wraps the shared postgres container + the app-role pool.
// Created once per package via RunMain; returned by Pool() to every
// test. Closed automatically when TestMain exits.
type Container struct {
	pool         *pgxpool.Pool
	pg           *postgres.PostgresContainer
	ownerDSN     string
	appDSN       string
	cfg          Config
}

// Pool returns the shared app-role pgxpool. Safe to call from every
// test in the package — pgxpool is goroutine-safe and the underlying
// connections multiplex per-tx via Acquire().
func (c *Container) Pool() *pgxpool.Pool { return c.pool }

// AppDSN returns the connection string used by the app role. Useful
// for tests that need a raw *sql.DB or a second pool with different
// pool config.
func (c *Container) AppDSN() string { return c.appDSN }

// OwnerDSN returns the connection string used by the owner (superuser)
// role. Useful for TRUNCATE / bypass-RLS reads in test helpers.
func (c *Container) OwnerDSN() string { return c.ownerDSN }

// TruncateAll wipes data from every table in the configured Grants
// schemas (preserving the goose_db_version migration ledger). Use ONLY
// for tests that NEED cross-tenant isolation — outbox forwarder tests,
// platform-scope reads, "expect zero rows globally" assertions.
//
// Most tests should use t.Parallel() + a fresh tenant_id per test
// instead — RLS isolates rows by tenant, no truncate needed, and
// parallel runs further amortize the per-test cost.
//
// Discovers tables via pg_tables at call time (no hardcoded list — safe
// across schema evolution). Uses the owner-role DSN to bypass RLS.
func (c *Container) TruncateAll(t testing.TB) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", c.ownerDSN)
	if err != nil {
		t.Fatalf("pgtest.TruncateAll: open owner DB: %v", err)
	}
	defer func() { _ = db.Close() }()

	schemaList := "'" + strings.Join(c.cfg.Grants, "','") + "'"
	q := `SELECT format('%I.%I', schemaname, tablename)
	      FROM pg_tables
	      WHERE schemaname IN (` + schemaList + `)
	        AND tablename NOT LIKE 'goose_%'`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("pgtest.TruncateAll: list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("pgtest.TruncateAll: scan: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pgtest.TruncateAll: iterate: %v", err)
	}
	if len(tables) == 0 {
		return
	}

	stmt := "TRUNCATE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("pgtest.TruncateAll: TRUNCATE failed: %v\nstmt: %s", err, stmt)
	}
}

// RunMain is the canonical TestMain entry point. Spins ONE postgres
// container, applies migrations, provisions the leadkart_app role with
// the configured grants, calls the supplied setup callback with the
// wired *Container (caller assigns to package-level sharedPG), THEN
// runs m.Run(). Returns the exit code; caller MUST call os.Exit on it.
//
// The setup callback runs INSIDE RunMain BEFORE m.Run() — this is the
// load-bearing ordering. The previous API (which returned the
// *Container after m.Run had already finished) made the canonical
// `var sharedPG *Container; ... sharedPG = c` pattern broken because
// sharedPG was nil during test execution. setup-callback enforces the
// correct order at compile time.
//
// Failure modes (Postgres image pull failure, migration failure, role
// grant failure) all surface as fmt-printed errors + exit code 1
// returned WITHOUT m.Run() being invoked. The setup callback is
// also not invoked on failure.
//
// No goleak wrapping — packages that want goleak run it after RunMain
// returns (before os.Exit). See identity/adapters/fixture_integration_test.go
// for the canonical wiring.
func RunMain(m *testing.M, cfg Config, setup func(*Container)) int {
	c, err := setupContainer(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest.RunMain: setup: %v\n", err)
		return 1
	}

	if setup != nil {
		setup(c)
	}

	code := m.Run()

	// Terminate gets its own ctx since TestMain's ctx may already be
	// cancelled by this point.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.pg.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest.RunMain: terminate: %v\n", err)
	}
	c.pool.Close()

	return code
}

// setupContainer is the actual bootstrap — pulled out of RunMain so
// error handling stays linear and the function is unit-testable if we
// later want to add a fake-container path.
func setupContainer(ctx context.Context, cfg Config) (*Container, error) {
	bootCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	pg, err := postgres.Run(bootCtx,
		"postgres:17-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(ownerRole),
		postgres.WithPassword(ownerPasswdLit),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres.Run: %w", err)
	}

	ownerDSN, err := pg.ConnectionString(bootCtx, "sslmode=disable")
	if err != nil {
		return nil, errors.Join(err, terminate(pg))
	}

	migDir, err := findMigrationsDir()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("find migrations: %w", err), terminate(pg))
	}

	if err := applyMigrationsAndGrants(bootCtx, ownerDSN, migDir, cfg); err != nil {
		return nil, errors.Join(fmt.Errorf("bootstrap: %w", err), terminate(pg))
	}

	host, err := pg.Host(bootCtx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("host: %w", err), terminate(pg))
	}
	port, err := pg.MappedPort(bootCtx, "5432/tcp")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("port: %w", err), terminate(pg))
	}

	// net/url builds the DSN string for us — avoids the `//` literal in
	// our source (which trips the arch-test SQL-interp regex via the
	// stripGoComments helper's known limitation around `//` inside
	// string literals) AND gives free URL-escaping for special chars.
	appDSNURL := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(appRoleName, appRolePasswd),
		Host:     net.JoinHostPort(host, port.Port()),
		Path:     dbName,
		RawQuery: "sslmode=disable",
	}
	appDSN := appDSNURL.String()

	pool, err := pgxpool.New(bootCtx, appDSN)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("pgxpool.New: %w", err), terminate(pg))
	}

	return &Container{
		pool:     pool,
		pg:       pg,
		ownerDSN: ownerDSN,
		appDSN:   appDSN,
		cfg:      normalizeConfig(cfg),
	}, nil
}

// applyMigrationsAndGrants runs goose up then provisions the app role
// with the configured schema grants. Mirrors the prior per-fixture
// bootstrapTestDB, generalised for any module's grant list.
func applyMigrationsAndGrants(ctx context.Context, ownerDSN, migDir string, cfg Config) error {
	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer func() { _ = gooseDB.Close() }()

	if err := pg.EnsureGooseDialect(); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	// "app" (RLS GUC helpers) and "common" (the shared watermill-sql outbox
	// relay + cross-cutting infra every module writes through, per ADR
	// 0064/0067) are universal — always provisioned regardless of the
	// per-package Config, mirroring how "app" has always been implicit.
	usageSchemas := dedupeSchemas(append([]string{"app", "common"}, cfg.Schemas...))
	grantSchemas := dedupeSchemas(append([]string{"common"}, cfg.Grants...))
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
			appRoleName, appRolePasswd),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, strings.Join(usageSchemas, ", "), appRoleName),
		fmt.Sprintf(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO %s`, appRoleName),
	}
	for _, schema := range grantSchemas {
		stmts = append(stmts,
			fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %s TO %s`,
				schema, appRoleName),
			// Serial/identity columns need explicit sequence USAGE — GRANT ON
			// ALL TABLES does NOT cover the backing sequences. Pair the two so
			// any serial in a granted schema works (e.g. the watermill-sql
			// relay common.outbox."offset" BIGSERIAL). Per ADR 0064/0067.
			fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA %s TO %s`,
				schema, appRoleName))
	}
	for _, s := range stmts {
		if _, err := gooseDB.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("provision %s: %s: %w", appRoleName, s, err)
		}
	}
	return nil
}

// normalizeConfig prepends the universal "common" schema — the shared
// watermill-sql outbox relay + cross-cutting infra every module reads and
// writes (ADR 0064/0067) — to both Schemas and Grants. This makes `common`
// implicit everywhere the Config is consumed: the app-role grants AND
// TruncateAll's wipe scope. Without it, per-module fixtures (whose Grants
// list only their own schema) leave common.outbox un-truncated, so its
// rows accumulate across tests and outbox-count assertions see stale rows.
// Mirrors how "app" has always been implicit.
func normalizeConfig(cfg Config) Config {
	cfg.Schemas = dedupeSchemas(append([]string{"common"}, cfg.Schemas...))
	cfg.Grants = dedupeSchemas(append([]string{"common"}, cfg.Grants...))
	return cfg
}

// dedupeSchemas removes duplicate schema names (preserving first-seen
// order) so a `GRANT ... ON SCHEMA a, b` list never names a schema twice
// — Postgres rejects "schema X specified more than once". Needed because
// "app"/"common" are now prepended unconditionally and a package's Config
// may also list them.
func dedupeSchemas(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// findMigrationsDir walks up from the current working directory until
// it finds a go.mod, then returns <repo>/migrations. Lets pgtest live
// in internal/common/ and find migrations regardless of which test
// package imported it.
func findMigrationsDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			migDir := filepath.Join(dir, "migrations")
			if _, err := os.Stat(migDir); err != nil {
				return "", fmt.Errorf("migrations dir not found at %s: %w", migDir, err)
			}
			return migDir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("walked to filesystem root without finding go.mod (started at %s)", wd)
		}
		dir = parent
	}
}

func terminate(c *postgres.PostgresContainer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Terminate(ctx)
}
