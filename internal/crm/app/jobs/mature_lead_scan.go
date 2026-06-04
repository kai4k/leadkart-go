// Package jobs holds CRM background-job workers per ADR 0010 (river).
//
// Per ADR 0047 boundary discipline: this package may NOT import
// internal/crm/adapters/db, pgx, pgxpool, pgtype, or
// internal/crm/adapters. Workers declare a CONSUMER-side Reader
// interface and the composition root wires a concrete pg-backed impl
// from internal/crm/adapters/ at boot time.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// MatureLeadThreshold is the BRD §4.7 cutoff: converted leads with no
// reorder activity within this window become mature-lead candidates.
// Constant at v0.2 per the BRD ("Threshold fixed at 3 months").
//
// v0.2 approximation: because crmlead doesn't track reorders at slice
// 1, the scanner uses crm.crm_leads.converted_at < (now - threshold)
// as the rough proxy. v0.3 swaps in last_reorder_at when the Orders
// module ships.
const MatureLeadThreshold = 90 * 24 * time.Hour

// MatureLeadScanInterval is the periodic run cadence. Daily at 03:00
// UTC per the slice brief — once a day matches the BRD's notification
// cadence without noising operator dashboards.
const MatureLeadScanInterval = 24 * time.Hour

// MatureLeadScanLimit caps the number of leads pulled per scan run so
// large tenants don't pin worker time. Subsequent runs pick up the
// rest (the partial unique index makes re-runs idempotent against
// already-minted reminders).
const MatureLeadScanLimit = 1000

// MatureLeadCandidate is the per-lead payload the [LeadScanner]
// returns. Strictly typed at this boundary — the concrete pg-backed
// reader in internal/crm/adapters/ converts sqlc rows into this VO.
type MatureLeadCandidate struct {
	TenantID               tenant.ID
	LeadID                 crmlead.ID
	AssignedToMembershipID string
	ConvertedAt            time.Time
}

// LeadScanner is the consumer-side interface the [MatureLeadScanJob]
// depends on. The composition root wires a concrete pg-backed
// implementation from internal/crm/adapters/ at boot.
type LeadScanner interface {
	// MatureLeads returns up to `limit` converted leads whose
	// converted_at predates `cutoff`. Runs cross-tenant under
	// TxScopePlatform so the scanner doesn't have to know the tenant
	// list up-front. The returned slice carries the per-lead
	// tenant_id; the worker dispatches the CreateReminderCommand
	// per-row under the correct scope.
	MatureLeads(ctx context.Context, cutoff time.Time, limit int) ([]MatureLeadCandidate, error)
}

// MatureLeadScanJob is the [river.JobArgs] payload. Empty by design —
// the job carries no per-row state; one run handles the entire scan.
// Periodic scheduling is set up by cmd/worker via river.NewPeriodicJob.
type MatureLeadScanJob struct{}

// Kind returns the river-stable job kind. Never rename — pre-existing
// scheduled jobs in the queue would orphan.
func (MatureLeadScanJob) Kind() string { return "crm.mature_lead_scan" }

// MatureLeadScanWorker is the [river.Worker] for [MatureLeadScanJob].
// Reuses [command.CreateReminderHandler] for the per-row reminder
// creation — the partial unique index on (tenant_id, lead_id) WHERE
// type='mature_lead' AND state='pending' guarantees the scan is
// idempotent against already-minted reminders.
type MatureLeadScanWorker struct {
	river.WorkerDefaults[MatureLeadScanJob]
	scanner LeadScanner
	create  command.CreateReminderHandler
	log     *slog.Logger
	now     func() time.Time
}

// NewMatureLeadScanWorker wires the worker. All args required.
// `now` is the injected clock per the clock-injection refactor; nil →
// time.Now.
func NewMatureLeadScanWorker(scanner LeadScanner, create command.CreateReminderHandler, log *slog.Logger, now func() time.Time) *MatureLeadScanWorker {
	if scanner == nil {
		panic("jobs: NewMatureLeadScanWorker scanner required")
	}
	if log == nil {
		panic("jobs: NewMatureLeadScanWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &MatureLeadScanWorker{
		scanner: scanner,
		create:  create,
		log:     log,
		now:     now,
	}
}

// Work runs the scan + dispatches a CreateReminderCommand per matching
// lead. Errors from individual reminder writes are logged + skipped
// (not propagated to River) so one malformed row does not block the
// rest of the batch; the river-level error log is reserved for the
// scan query itself.
func (w *MatureLeadScanWorker) Work(ctx context.Context, _ *river.Job[MatureLeadScanJob]) error {
	now := w.now()
	cutoff := now.Add(-MatureLeadThreshold)
	candidates, err := w.scanner.MatureLeads(ctx, cutoff, MatureLeadScanLimit)
	if err != nil {
		return fmt.Errorf("crm mature-lead scan: %w", err)
	}
	created := 0
	duplicates := 0
	failures := 0
	due := now.Add(MatureLeadThreshold) // give the assignee a window equal to the threshold to act
	for _, c := range candidates {
		out, err := w.create.Handle(ctx, command.CreateReminderCommand{
			TenantID:               c.TenantID,
			LeadID:                 c.LeadID,
			AssignedToMembershipID: c.AssignedToMembershipID,
			Type:                   reminder.TypeMatureLead,
			DueAt:                  due,
			Notes:                  matureLeadNotes(c, cutoff),
		})
		switch {
		case err == nil && out.AlreadyExisted:
			duplicates++
		case err == nil:
			created++
		case errors.Is(err, command.ErrLeadNotFound):
			// Lead disappeared between scan + reminder write. Benign —
			// the row was deleted or moved tenant. Log + skip.
			w.log.WarnContext(ctx, "crm mature-lead scan: lead disappeared mid-run",
				"lead_id", c.LeadID.String(), "tenant_id", c.TenantID.String())
			failures++
		default:
			w.log.ErrorContext(ctx, "crm mature-lead scan: create reminder",
				"lead_id", c.LeadID.String(), "tenant_id", c.TenantID.String(),
				"err", err)
			failures++
		}
	}
	w.log.InfoContext(ctx, "crm mature-lead scan complete",
		"cutoff", cutoff.Format(time.RFC3339),
		"scanned", len(candidates),
		"created", created,
		"duplicates", duplicates,
		"failures", failures,
	)
	return nil
}

func matureLeadNotes(c MatureLeadCandidate, cutoff time.Time) string {
	return fmt.Sprintf("mature-lead alert: converted %s; no reorder activity since cutoff %s",
		c.ConvertedAt.Format("2006-01-02"),
		cutoff.Format("2006-01-02"),
	)
}
