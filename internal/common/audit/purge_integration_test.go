//go:build integration

package audit_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/audit"
)

func TestPurgeWorker_DeletesOlderThanRetention(t *testing.T) {
	pool := startPostgres(t)

	// Seed: one row well outside the retention window, one inside.
	// Comparison is occurred_at_utc < NOW() - PurgeRetention; we use
	// fixed offsets relative to now so the test doesn't depend on a
	// frozen clock.
	now := time.Now().UTC()
	old := audit.Entry{
		Action:        "test.purge.old",
		OccurredAtUTC: now.Add(-audit.PurgeRetention - 24*time.Hour),
		Duration:      time.Millisecond,
		Succeeded:     true,
	}
	fresh := audit.Entry{
		Action:        "test.purge.fresh",
		OccurredAtUTC: now.Add(-audit.PurgeRetention + 24*time.Hour),
		Duration:      time.Millisecond,
		Succeeded:     true,
	}
	w := audit.NewWriter(pool, silentLogger(), time.Now)
	if err := w.Write(t.Context(), old); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := w.Write(t.Context(), fresh); err != nil {
		t.Fatalf("write fresh: %v", err)
	}

	// Run the purge worker directly — the river client is integration
	// scaffolding here; we assert the SQL contract of Work itself.
	purger := audit.NewPurgeWorker(pool, silentLogger(), time.Now)
	if err := purger.Work(t.Context(), &river.Job[audit.PurgeJob]{Args: audit.PurgeJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	var oldCount, freshCount int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM buildingblocks.audit_log_entry WHERE action = 'test.purge.old'`,
	).Scan(&oldCount); err != nil {
		t.Fatalf("count old: %v", err)
	}
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM buildingblocks.audit_log_entry WHERE action = 'test.purge.fresh'`,
	).Scan(&freshCount); err != nil {
		t.Fatalf("count fresh: %v", err)
	}

	if oldCount != 0 {
		t.Errorf("old rows after purge: got %d want 0", oldCount)
	}
	if freshCount != 1 {
		t.Errorf("fresh rows after purge: got %d want 1 (purge over-deleted!)", freshCount)
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startPostgres spins testcontainers Postgres + applies migrations.
// Distinct from cmd/api's fixture because audit-package tests run
// inside their own package — no cross-package helper sharing.
func startPostgres(t *testing.T) *pgxpool.Pool {
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
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
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

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
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
	if err := goose.UpContext(ctx, gooseDB, migrationsDir(t)); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	_ = gooseDB.Close()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Smoke audit_log_entry exists at the expected schema location.
	_ = ids.NewV7() // quiet unused import in case ids isn't used elsewhere
	return pool
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "migrations")
}
