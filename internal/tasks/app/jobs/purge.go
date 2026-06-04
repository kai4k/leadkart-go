package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// PurgeRetention is the soft-delete window for terminal work items
// per BRD §6.8 — completed + cancelled tasks survive 3 months for
// audit + dashboard "completed_today" counts, then drop.
const PurgeRetention = 90 * 24 * time.Hour

// PurgeBatchSize caps the rows scanned per tick.
const PurgeBatchSize = 1_000

// PurgeInterval is the periodic run interval per BRD §6.8 (daily).
const PurgeInterval = 24 * time.Hour

// PurgeJob is the river.JobArgs payload.
type PurgeJob struct{}

// Kind returns the river-stable job kind.
func (PurgeJob) Kind() string { return "tasks.purge" }

// PurgeWorker is the river.Worker for [PurgeJob]. Per tick: lists
// terminal work items whose terminal timestamp is older than
// PurgeRetention + soft-deletes each.
type PurgeWorker struct {
	river.WorkerDefaults[PurgeJob]

	repo workitem.Repository
	log  *slog.Logger
	now  func() time.Time
}

// NewPurgeWorker wires the worker.
func NewPurgeWorker(repo workitem.Repository, log *slog.Logger, now func() time.Time) *PurgeWorker {
	if repo == nil {
		panic("jobs: NewPurgeWorker repo required")
	}
	if log == nil {
		panic("jobs: NewPurgeWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &PurgeWorker{repo: repo, log: log, now: now}
}

// Work executes one purge pass.
func (w *PurgeWorker) Work(ctx context.Context, _ *river.Job[PurgeJob]) error {
	cutoff := w.now().UTC().Add(-PurgeRetention)
	candidates, err := w.repo.ListPurgeCandidates(ctx, tenant.ID(""), cutoff, PurgeBatchSize)
	if err != nil {
		return fmt.Errorf("tasks purge: list: %w", err)
	}
	deleted := 0
	for _, c := range candidates {
		if err := w.repo.DeleteByID(ctx, c.TenantID(), c.ID()); err != nil {
			w.log.ErrorContext(ctx, "tasks purge: soft delete",
				"err", err, "work_item_id", c.ID().String(), "tenant_id", c.TenantID().String())
			continue
		}
		deleted++
	}
	w.log.InfoContext(ctx, "tasks purge complete",
		"cutoff", cutoff.Format(time.RFC3339), "candidates", len(candidates), "deleted", deleted,
		"retention", PurgeRetention.String())
	return nil
}
