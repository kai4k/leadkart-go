//go:build integration

// arch-test:no-timeout-needed — single test in this file uses the shared
//   pgtest container; pgxpool internal conn timeouts + package-level
//   `task ci:test:int -timeout=15m` already bound execution.

package audit_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/audit/audittest"
)

func TestPurgeWorker_DeletesOlderThanRetention(t *testing.T) {
	// arch-test:no-parallel — asserts on global audit_log_entry row counts
	// (cross-tenant by design); uses TruncateAll for clean state.
	sharedPG.TruncateAll(t)
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

	oldCount := audittest.CountByAction(t, pool, "test.purge.old")
	freshCount := audittest.CountByAction(t, pool, "test.purge.fresh")

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

// Shared bootstrap (startPostgres / TestMain) lives in
// fixture_integration_test.go per the Brandur / TDL canon.
