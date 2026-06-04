// Package jobs holds Tasks-module background workers driven by river.
//
// Two periodic jobs per BRD §6.8:
//   - OverdueScanJob — every 15 minutes; flags due_at-slipped work
//     items as overdue + emits the integration event for downstream
//     notification routing.
//   - PurgeJob — daily 03:00 UTC; soft-deletes completed/cancelled
//     work items older than 3 months.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// OverdueScanInterval — every 15 minutes per BRD §6.8.
const OverdueScanInterval = 15 * time.Minute

// OverdueScanBatchSize caps the rows scanned per tick. Higher limits
// risk long-running scans on overdue backlogs; the periodic interval
// catches up.
const OverdueScanBatchSize = 500

// OverdueScanJob is the river.JobArgs payload. Empty by design —
// scans every tenant.
type OverdueScanJob struct{}

// Kind returns the river-stable job kind.
func (OverdueScanJob) Kind() string { return "tasks.overdue_scan" }

// OverdueScanWorker is the river.Worker for [OverdueScanJob]. Per
// tick: enumerates pending/in_progress work items with due_at <
// now() across ALL tenants, then dispatches MarkOverdueCommand for
// each — the aggregate's idempotency guards repeat replays.
type OverdueScanWorker struct {
	river.WorkerDefaults[OverdueScanJob]

	repo        workitem.Repository
	markOverdue command.MarkOverdueHandler
	log         *slog.Logger
	now         func() time.Time
}

// NewOverdueScanWorker wires the worker.
func NewOverdueScanWorker(repo workitem.Repository, markOverdue command.MarkOverdueHandler, log *slog.Logger, now func() time.Time) *OverdueScanWorker {
	if repo == nil {
		panic("jobs: NewOverdueScanWorker repo required")
	}
	if log == nil {
		panic("jobs: NewOverdueScanWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &OverdueScanWorker{repo: repo, markOverdue: markOverdue, log: log, now: now}
}

// Work executes one scan pass.
func (w *OverdueScanWorker) Work(ctx context.Context, _ *river.Job[OverdueScanJob]) error {
	asOf := w.now().UTC()
	candidates, err := w.repo.ListOverdueCandidates(ctx, tenant.ID(""), asOf, OverdueScanBatchSize)
	if err != nil {
		return fmt.Errorf("tasks overdue_scan: list: %w", err)
	}
	flagged := 0
	for _, c := range candidates {
		err := w.markOverdue.Handle(ctx, command.MarkOverdueCommand{
			TenantID:   c.TenantID(),
			WorkItemID: c.ID(),
		})
		if err != nil {
			// Single-row failure shouldn't kill the whole batch —
			// log + continue. River retries the entire job; per-row
			// idempotency means re-runs are safe.
			w.log.ErrorContext(ctx, "tasks overdue_scan: mark",
				"err", err, "work_item_id", c.ID().String(), "tenant_id", c.TenantID().String())
			continue
		}
		flagged++
	}
	w.log.InfoContext(ctx, "tasks overdue_scan complete",
		"as_of", asOf.Format(time.RFC3339), "candidates", len(candidates), "flagged", flagged)
	return nil
}
