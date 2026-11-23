// Package jobs holds the companysettings river background worker (ADR 0010,
// ADR 0070): the daily republish of every stored policy.
//
// Orders and Inventory keep event-fed replicas of each company's policy. A
// dead-lettered event would leave a replica stale until the next change; this
// job re-announces every tenant's CURRENT stored version once a day, so the
// replicas converge. Replicas store a version only when it is newer, so the
// re-announcement of a version they already hold changes nothing.
//
// Boundary discipline (ADR 0047): the worker depends on the [TenantLister]
// interface declared here and the republish command; the pgx adapter lives in
// internal/companysettings/adapters.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// JobTimeout caps the republish run: one primary-key read, one outbox row per
// tenant.
const JobTimeout = 10 * time.Minute

// RepublishInterval is how often the periodic job runs.
const RepublishInterval = 24 * time.Hour

// PageSize is how many tenants one listing returns.
const PageSize = 200

// TenantLister lists the tenants that have a stored policy (version 1 or later),
// ordered by id, after the given id. The adapter runs it under platform scope:
// the republish sweep is the one cross-tenant read.
type TenantLister interface {
	ListStoredTenants(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Republisher re-announces one tenant's stored policy under that tenant's scope.
type Republisher interface {
	Handle(ctx context.Context, cmd command.RepublishCommand) error
}

var _ Republisher = command.RepublishHandler{}

// RepublishJob is the periodic job. It carries no per-row state.
//
// river:idempotent — re-announcing a version is a no-op for a replica that
// already holds it (stored only when newer), so a retry or an overlapping run is
// safe.
type RepublishJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (RepublishJob) Kind() string { return "companysettings.republish_policies" }

// RepublishWorker runs [RepublishJob].
type RepublishWorker struct {
	river.WorkerDefaults[RepublishJob]
	tenants   TenantLister
	republish Republisher
	log       *slog.Logger
}

// NewRepublishWorker wires the worker.
func NewRepublishWorker(tenants TenantLister, republish Republisher, log *slog.Logger) *RepublishWorker {
	if tenants == nil || republish == nil {
		panic("jobs: NewRepublishWorker tenants and republish required")
	}
	if log == nil {
		panic("jobs: NewRepublishWorker log required")
	}
	return &RepublishWorker{tenants: tenants, republish: republish, log: log}
}

// Timeout enforces JobTimeout per river canon.
func (*RepublishWorker) Timeout(*river.Job[RepublishJob]) time.Duration { return JobTimeout }

// Work walks every stored tenant page by page and re-announces its policy. One
// tenant's failure is logged and does not stop the sweep; the job then returns
// an error so river retries it (re-announcing is idempotent).
func (w *RepublishWorker) Work(ctx context.Context, _ *river.Job[RepublishJob]) error {
	var failed []error
	total := 0
	after := uuid.Nil
	for {
		page, err := w.tenants.ListStoredTenants(ctx, after, PageSize)
		if err != nil {
			return fmt.Errorf("republish policies: list tenants after %s: %w", after, err)
		}
		for _, id := range page {
			if err := w.republish.Handle(ctx, command.RepublishCommand{TenantID: tenant.ID(id.String())}); err != nil {
				w.log.ErrorContext(ctx, "companysettings: republish failed", "tenant_id", id.String(), "err", err)
				failed = append(failed, fmt.Errorf("tenant %s: %w", id, err))
				continue
			}
			total++
		}
		if len(page) < PageSize {
			break
		}
		after = page[len(page)-1]
	}
	w.log.InfoContext(ctx, "companysettings: policies republished", "tenants", total, "failed", len(failed))
	if len(failed) > 0 {
		return fmt.Errorf("republish policies: %d tenant(s) failed: %w", len(failed), errors.Join(failed...))
	}
	return nil
}
