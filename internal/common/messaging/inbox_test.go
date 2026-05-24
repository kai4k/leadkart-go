//go:build integration

package messaging_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// inboxFixture spins ephemeral Postgres + applies migrations + returns
// a pgxpool. Container auto-cleans via t.Cleanup. Connects as the
// (default) superuser — the inbox table is non-RLS so role choice
// doesn't matter for these tests.
func inboxFixture(t *testing.T) *pgxpool.Pool {
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
		// Cleanup runs after t.Context() is cancelled — must use Background.
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
	defer gooseDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	if err := goose.UpContext(ctx, gooseDB, migrationsDir(t)); err != nil {
		t.Fatalf("goose up: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
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

func TestIdempotentReceiver_FirstCall_RunsHandlerAndRecords(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	calls := &atomic.Int32{}
	wrapped := receiver.Wrap("test.handler", func(ctx context.Context, mid string) error {
		calls.Add(1)
		return nil
	})

	if err := wrapped(t.Context(), "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls: got %d want 1", calls.Load())
	}

	// Verify row exists.
	var n int
	err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM identity.processed_messages
		WHERE  message_id = $1 AND handler_name = $2
	`, "11111111-1111-1111-1111-111111111111", "test.handler").Scan(&n)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed_messages row count: got %d want 1", n)
	}
}

func TestIdempotentReceiver_Replay_SkipsHandler(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	calls := &atomic.Int32{}
	wrapped := receiver.Wrap("test.handler", func(ctx context.Context, mid string) error {
		calls.Add(1)
		return nil
	})

	mid := "22222222-2222-2222-2222-222222222222"
	for i := range 5 {
		if err := wrapped(t.Context(), mid); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls after 5 replays: got %d want 1", calls.Load())
	}
}

func TestIdempotentReceiver_HandlerError_DoesNotRecord_NextCallRunsAgain(t *testing.T) {
	t.Parallel()
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	mid := "33333333-3333-3333-3333-333333333333"
	calls := &atomic.Int32{}
	flaky := receiver.Wrap("test.flaky", func(ctx context.Context, _ string) error {
		n := calls.Add(1)
		if n == 1 {
			return errors.New("transient")
		}
		return nil
	})

	// 1. Handler errors → no dedup row recorded.
	err := flaky(t.Context(), mid)
	if err == nil || err.Error() != "transient" {
		t.Fatalf("first call expected transient err, got %v", err)
	}
	var n int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM identity.processed_messages
		WHERE  message_id = $1 AND handler_name = $2
	`, mid, "test.flaky").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("dedup row recorded after handler error: got %d want 0", n)
	}

	// 2. Retry succeeds — handler runs again, dedup row recorded.
	if err := flaky(t.Context(), mid); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls after retry: got %d want 2", calls.Load())
	}

	// 3. Third call replays — handler does NOT run.
	if err := flaky(t.Context(), mid); err != nil {
		t.Fatalf("third: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("third call ran handler again: got %d want 2", calls.Load())
	}
}

func TestIdempotentReceiver_ScopedByHandlerName(t *testing.T) {
	t.Parallel()
	// Same message_id processed by two distinct handlers — both run.
	pool := inboxFixture(t)
	receiver := messaging.NewIdempotentReceiver(pool)

	a, b := atomic.Int32{}, atomic.Int32{}
	hA := receiver.Wrap("test.handlerA", func(context.Context, string) error {
		a.Add(1)
		return nil
	})
	hB := receiver.Wrap("test.handlerB", func(context.Context, string) error {
		b.Add(1)
		return nil
	})

	mid := "44444444-4444-4444-4444-444444444444"
	for _, h := range []messaging.HandlerFunc{hA, hB, hA, hB} {
		if err := h(t.Context(), mid); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	if a.Load() != 1 || b.Load() != 1 {
		t.Fatalf("counts a=%d b=%d, want 1/1", a.Load(), b.Load())
	}
}

func TestIdempotentReceiver_Wrap_PanicsOnEmptyName(t *testing.T) {
	t.Parallel()
	receiver := messaging.NewIdempotentReceiver(nil) // pool not used in this path
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty handlerName")
		}
	}()
	_ = receiver.Wrap("", func(context.Context, string) error { return nil }) // arch-test:ignore-err — asserts panic; return value unreachable
}
