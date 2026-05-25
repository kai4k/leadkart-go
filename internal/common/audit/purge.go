package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// PurgeRetention is the retention window enforced by [PurgeJob].
// Per `data-retention.md` "Audit log retention" — 7 years, driven by
// SOC2 CC4.1 + Indian tax/regulatory record-keeping rules for pharma.
//
// Rows older than NOW() - PurgeRetention are deleted on every run.
// Cold-storage export to S3 Glacier (Pre-purge per SOC2) lands in a
// future commit; v0.2 just deletes.
const PurgeRetention = 7 * 365 * 24 * time.Hour

// PurgeJob is a [river.JobArgs] payload the worker pool processes.
// Empty by design — the job carries no per-row state; it operates
// against a constant retention window. Periodic scheduling is set up
// by cmd/worker via river.NewPeriodicJob.
type PurgeJob struct{}

// Kind returns the river-stable job kind. Never rename — pre-existing
// scheduled jobs in the queue would orphan.
func (PurgeJob) Kind() string { return "audit.purge" }

// PurgeWorker is the river.Worker for [PurgeJob]. Holds the pool +
// logger so the Work method can issue the bulk DELETE without going
// through the application repos (which carry per-tenant scope this
// job intentionally bypasses — purging is a cross-tenant operator
// action).
type PurgeWorker struct {
	river.WorkerDefaults[PurgeJob]
	pool *pgxpool.Pool
	log  *slog.Logger
	now  func() time.Time
}

// NewPurgeWorker wires the worker against pool. log is required for
// the run-summary log line. `now` is the explicit time source per
// the clock-injection refactor — composition root wires `time.Now`.
// Nil → time.Now.
func NewPurgeWorker(pool *pgxpool.Pool, log *slog.Logger, now func() time.Time) *PurgeWorker {
	if pool == nil {
		panic("audit: NewPurgeWorker pool required")
	}
	if log == nil {
		panic("audit: NewPurgeWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &PurgeWorker{pool: pool, log: log, now: now}
}

// Work executes the DELETE. Returns the row count via a structured
// log entry; River captures the error (if any) onto the job's
// metadata.
//
// SQL is kept inline (not via sqlc) because the retention window is
// a constant, the table is admin-owned, and a one-line DELETE
// doesn't justify the codegen ceremony. Per `coding-standards.md`
// "raw SQL acceptable for ops queries against admin tables".
func (w *PurgeWorker) Work(ctx context.Context, _ *river.Job[PurgeJob]) error {
	// Injected clock per the post-Wave-9 clock-injection refactor:
	// production wires time.Now; tests inject a fixed-time closure
	// so the cutoff stays deterministic.
	cutoff := w.now().Add(-PurgeRetention)
	tag, err := w.pool.Exec(ctx, `
		DELETE FROM buildingblocks.audit_log_entry
		WHERE occurred_at_utc < $1
	`, cutoff)
	if err != nil {
		return fmt.Errorf("audit purge: %w", err)
	}
	w.log.InfoContext(ctx, "audit purge complete",
		"rows_deleted", tag.RowsAffected(),
		"cutoff", cutoff.Format(time.RFC3339),
		"retention", PurgeRetention.String(),
	)
	return nil
}

// PurgeInterval is the periodic run interval for [PurgeJob]. Daily
// per `data-retention.md`: enough to keep the table from accumulating
// dead rows, infrequent enough not to noise up operator dashboards
// or Postgres autovacuum stats.
const PurgeInterval = 24 * time.Hour
