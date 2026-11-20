// Package jobs holds the Identity module's River background jobs.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// LoginPhantomPurgeInterval is how often the purge runs. Five minutes keeps the
// table within one lockout window of its live size (ADR 0068 D12; the migration
// states the worst-case row count under the per-client limits).
const LoginPhantomPurgeInterval = 5 * time.Minute

// LoginPhantomPurgeBatchSize bounds one DELETE; the job repeats until a batch
// comes back short, so a backlog drains without one long-held lock.
const LoginPhantomPurgeBatchSize = 5_000

// loginPhantomPurgeMaxBatches caps one run so a flood cannot pin the worker: the
// next tick continues. 200 x 5,000 = 1,000,000 rows a run.
const loginPhantomPurgeMaxBatches = 200

// loginPhantomPurgeTimeout is the River per-job timeout.
const loginPhantomPurgeTimeout = 2 * time.Minute

// LoginPhantomPurgeJob is the river.JobArgs payload (empty: it works against
// constants).
type LoginPhantomPurgeJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (LoginPhantomPurgeJob) Kind() string { return "identity.login_phantom_purge" }

// PhantomPurger deletes up to batch dead rows of the phantom failed-login counter
// (window and lock both passed) and returns how many. Consumer-defined; the
// Postgres adapter implements it.
type PhantomPurger interface {
	PurgeExpired(ctx context.Context, now time.Time, batch int) (int64, error)
}

// LoginPhantomPurgeWorker is the river.Worker for [LoginPhantomPurgeJob].
//
// river:idempotent - deleting rows that are already dead is a no-op, so a retry or
// an overlapping run only finds less to delete.
type LoginPhantomPurgeWorker struct {
	river.WorkerDefaults[LoginPhantomPurgeJob]

	purger PhantomPurger
	log    *slog.Logger
	now    func() time.Time
}

// NewLoginPhantomPurgeWorker wires the worker. now is the explicit clock (nil =
// time.Now).
func NewLoginPhantomPurgeWorker(purger PhantomPurger, log *slog.Logger, now func() time.Time) *LoginPhantomPurgeWorker {
	if purger == nil {
		panic("jobs: NewLoginPhantomPurgeWorker purger required")
	}
	if log == nil {
		panic("jobs: NewLoginPhantomPurgeWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &LoginPhantomPurgeWorker{purger: purger, log: log, now: now}
}

// Timeout bounds one run.
func (*LoginPhantomPurgeWorker) Timeout(*river.Job[LoginPhantomPurgeJob]) time.Duration {
	return loginPhantomPurgeTimeout
}

// Work deletes dead rows in bounded batches until a batch is short.
func (w *LoginPhantomPurgeWorker) Work(ctx context.Context, _ *river.Job[LoginPhantomPurgeJob]) error {
	now := w.now()
	var total int64
	for range loginPhantomPurgeMaxBatches {
		n, err := w.purger.PurgeExpired(ctx, now, LoginPhantomPurgeBatchSize)
		if err != nil {
			return fmt.Errorf("login phantom purge: %w", err)
		}
		total += n
		if n < LoginPhantomPurgeBatchSize {
			break
		}
	}
	w.log.InfoContext(ctx, "login phantom purge complete", "rows_deleted", total)
	return nil
}
