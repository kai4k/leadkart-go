//go:build integration

package adapters_test

import (
	"context"
	"database/sql"
	"errors"
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

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
)

// platformPool spins an ephemeral Postgres, applies all migrations as
// the owner role, then provisions a non-superuser leadkart_app role
// with grants on schema platform + identity (the slice imports
// identity for the bootstrap), and returns a pgxpool bound as that
// role. RLS only fires when the connection is NOT a superuser.
//
// Mirror of identity's repoFixture per Wave 6 ADR + identity's
// testcontainers pattern. See internal/identity/adapters/
// tenant_repository_pg_test.go for the canonical version.
func platformPool(t *testing.T) *pgxpool.Pool {
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

	if err := bootstrapPlatformDB(ctx, ownerDSN, migrationsDir(t)); err != nil {
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

func bootstrapPlatformDB(ctx context.Context, ownerDSN, migrationsDir string) error {
	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("goose open: %w", err)
	}
	defer gooseDB.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	stmts := []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity, platform TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA platform TO leadkart_app`,
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
	// here = .../internal/platform/adapters/fixture_integration_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}

// openRawDB returns a database/sql handle pointed at the same DSN as
// the pgxpool — used for verification queries that bypass the
// repository (e.g. outbox-row inspection under platform GUC).
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := cfg.ConnString()
	if dsn == "" {
		return nil, errors.New("pool has no ConnString")
	}
	return sql.Open("pgx", dsn)
}

// fixtureForm returns a valid leadform.Form for integration-test seed.
func fixtureForm(t *testing.T) leadform.Form {
	t.Helper()
	f, err := leadform.New(leadform.Input{
		ContactName:    "Acme Pharma Integration",
		MobileE164:     "+919876543210",
		Email:          "ops@acme.test",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		HasDrugLicence: true,
		HasGst:         true,
		GstNumber:      "27AAAAA0000A1Z5",
		HasPan:         true,
		PanNumber:      "AAAAA0000A",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		ProductRanges:  []string{"Antibiotics"},
		DosageForms:    []string{"Tablet"},
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	})
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	return f
}

func nowUTC() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UTC()
}
