//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	identityadapters "github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedNow is the deterministic timestamp inventory integration tests
// pass to domain factories and mutators. Replaces the prior
// package-global clock per the clock-injection refactor — each test
// supplies the instant explicitly so no two parallel test files can
// race on a shared mutable clock.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// testNow is an alias for fixedNow, used by aggregate factories that the
// shared rewrite script standardised on testNow.
var testNow = fixedNow

// repoFixture spins an ephemeral Postgres + applies migrations + creates
// the non-superuser leadkart_app role with grants for the inventory
// schema (plus identity for the tenant FK).
//
// Mirror of internal/identity/adapters/tenant_repository_pg_test.go
// `repoFixture` — kept independent so the inventory test package can run
// without importing identity test internals.
func repoFixture(t *testing.T) *pgxpool.Pool {
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
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	ownerDSN, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	if err := bootstrapTestDB(ctx, ownerDSN, migrationsDir(t)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	host, port, err := containerHostPort(ctx, c)
	if err != nil {
		t.Fatalf("host:port: %v", err)
	}
	appDSN := "postgres://leadkart_app:leadkart_app_pw@" + host + ":" + port + "/leadkart_test?sslmode=disable"

	pool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func bootstrapTestDB(ctx context.Context, ownerDSN, migrationsDir string) error {
	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer gooseDB.Close()

	if err := pg.EnsureGooseDialect(); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	stmts := []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity, inventory TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA inventory TO leadkart_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app`,
	}
	for _, s := range stmts {
		if _, err := gooseDB.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("provision leadkart_app: %s: %w", s, err)
		}
	}
	return nil
}

func containerHostPort(ctx context.Context, c *postgres.PostgresContainer) (string, string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", "", fmt.Errorf("host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", "", fmt.Errorf("port: %w", err)
	}
	return host, port.Port(), nil
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../internal/inventory/adapters/fixture_integration_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}

// seedTenant inserts a fresh tenant row via the identity TenantRepository
// so inventory tests have a real tenant id satisfying the composite FK
// on inventory.products.
func seedTenant(t *testing.T, pool *pgxpool.Pool) tenant.ID {
	t.Helper()
	tx := pg.NewTransactor(pool)
	repo := identityadapters.NewTenantRepository(pool, tx)
	id := tenant.ID(ids.NewV7().String())
	full := ids.NewV7().String()
	s, err := slug.New("inv-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@inv.test")
	tn, err := tenant.New(id, s, "Inv Pharma Pvt Ltd", "Inv", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return id
}

// tenantCtx binds the tenant_id GUC for tenant-scoped repo calls.
func tenantCtx(t *testing.T, id tenant.ID) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(id.String()))
}

// openRawDB returns a database/sql handle pointed at the same DSN as the
// pool, useful for SELECTs that need to bypass the inventory adapter +
// flip platform-bypass for outbox checks.
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	return sql.Open("pgx", dsn)
}
