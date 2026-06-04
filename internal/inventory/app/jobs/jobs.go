// Package jobs holds Inventory module river background workers per
// ADR 0010 + BRD §6.5. Two daily jobs:
//
//   - ExpiryScanJob: scans batches nearing expiry, publishes
//     BatchExpiringSoonV1 (deduped per (tenant, batch, day)).
//   - ReorderScanJob: scans products below reorder_level, publishes
//     ProductBelowReorderLevelV1 (deduped per (tenant, product, day)).
//
// Both run platform-scope (no tenant GUC binding) so they see all
// tenants; dedup ledger is `inventory.alert_emissions`. The workers
// emit via the inventory outbox so downstream subscribers (Notifications)
// pick them up through the standard Watermill forwarder.
//
// Idempotency: ON CONFLICT DO NOTHING on the per-day PK means re-runs
// within the same UTC day are no-ops. river retry on failure is safe.
//
// Boundary discipline (ADR 0047): the workers depend on the [AlertScanRepo]
// interface declared here (consumer-side); the concrete pgx-backed
// implementation lives in `internal/inventory/adapters/`. No pgx /
// adapters/db imports leak into app/.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// JobTimeout caps any single job's runtime. Per-tenant scans are
// bounded by tenant count × per-tenant query cost; 10min is a generous
// ceiling for v0.2 scale (low-thousands of tenants).
const JobTimeout = 10 * time.Minute

// AlertKind constants — wire-stable strings stored in
// inventory.alert_emissions.kind.
const (
	AlertKindBatchExpiring       = "batch_expiring"
	AlertKindProductBelowReorder = "product_below_reorder"
)

// BatchExpiring is the framework-neutral projection the worker
// consumes. Mirrors adapters.TenantBatchExpiring shape but lives in
// the app layer so app/ doesn't import adapters/.
type BatchExpiring struct {
	TenantID      uuid.UUID
	ProductID     uuid.UUID
	BatchID       uuid.UUID
	BatchNumber   string
	ExpiryDate    time.Time
	ThresholdDays int
}

// ReorderProduct is the framework-neutral projection for the reorder
// scan.
type ReorderProduct struct {
	TenantID     uuid.UUID
	ProductID    uuid.UUID
	SKU          string
	ReorderLevel int
	StockOnHand  int64
}

// AlertScanRepo is the consumer-side interface the workers depend on.
// The concrete adapter lives in `internal/inventory/adapters/`; the
// composition root wires it in.
//
// Per TDL canon "accept interfaces, return structs" — handler-local
// interface, lives next to the consumer, never in a shared package.
type AlertScanRepo interface {
	// ListTenants returns every tenant with at least one live product.
	ListTenants(ctx context.Context) ([]uuid.UUID, error)
	// ListBatchesNearExpiry returns batches near expiry per the per-
	// product threshold.
	ListBatchesNearExpiry(ctx context.Context, tenantID uuid.UUID, today time.Time) ([]BatchExpiring, error)
	// ListProductsBelowReorder returns products below their reorder
	// level (live + not-expired stock counted).
	ListProductsBelowReorder(ctx context.Context, tenantID uuid.UUID, today time.Time) ([]ReorderProduct, error)
	// EmitIfNew atomically dedups + writes the integration event to
	// the inventory outbox. Returns (true, nil) when newly emitted.
	EmitIfNew(ctx context.Context, tenantID uuid.UUID, kind string, subjectID uuid.UUID, today time.Time, event integrationevents.Event) (bool, error)
}

// ---------------------------------------------------------------------------
// ExpiryScanJob
// ---------------------------------------------------------------------------

// ExpiryScanJob is the periodic job that scans inventory for batches
// approaching expiry. Carries no per-row state — args are empty.
//
// river:idempotent — dedup ledger (inventory.alert_emissions) PK on
// (tenant, kind, subject, day) makes ON CONFLICT DO NOTHING the
// per-row idempotency primitive. River retry on failure is safe.
type ExpiryScanJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (ExpiryScanJob) Kind() string { return "inventory.expiry_scan" }

// ExpiryScanWorker runs [ExpiryScanJob].
type ExpiryScanWorker struct {
	river.WorkerDefaults[ExpiryScanJob]
	repo AlertScanRepo
	log  *slog.Logger
	now  func() time.Time
}

// NewExpiryScanWorker wires the worker. `now` is the explicit time
// source — composition root wires `time.Now`; tests inject a closure.
func NewExpiryScanWorker(repo AlertScanRepo, log *slog.Logger, now func() time.Time) *ExpiryScanWorker {
	if repo == nil {
		panic("jobs: NewExpiryScanWorker repo required")
	}
	if log == nil {
		panic("jobs: NewExpiryScanWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &ExpiryScanWorker{repo: repo, log: log, now: now}
}

// Timeout enforces JobTimeout per river canon.
func (*ExpiryScanWorker) Timeout(*river.Job[ExpiryScanJob]) time.Duration { return JobTimeout }

// Work iterates all tenants, scans for near-expiry batches, dedups
// via alert_emissions, and emits BatchExpiringSoonV1 to inventory.outbox
// for each newly-detected row.
func (w *ExpiryScanWorker) Work(ctx context.Context, _ *river.Job[ExpiryScanJob]) error {
	now := w.now().UTC()
	today := truncateToDay(now)

	tenants, err := w.repo.ListTenants(ctx)
	if err != nil {
		return fmt.Errorf("expiry scan: list tenants: %w", err)
	}

	totalEmitted := 0
	for _, tid := range tenants {
		batches, err := w.repo.ListBatchesNearExpiry(ctx, tid, today)
		if err != nil {
			return fmt.Errorf("expiry scan: tenant %s rows: %w", tid, err)
		}
		for _, b := range batches {
			evt := integrationevents.BatchExpiringSoonV1{
				BatchID:         b.BatchID,
				ProductID:       b.ProductID,
				TenantIDClaim:   b.TenantID,
				BatchNumber:     b.BatchNumber,
				ExpiryDate:      b.ExpiryDate,
				DaysUntilExpiry: daysBetween(today, b.ExpiryDate),
				OccurredAtUTC:   now,
			}
			emitted, err := w.repo.EmitIfNew(ctx, b.TenantID, AlertKindBatchExpiring, b.BatchID, today, evt)
			if err != nil {
				return fmt.Errorf("expiry scan: emit %s: %w", b.BatchID, err)
			}
			if emitted {
				totalEmitted++
			}
		}
	}
	w.log.InfoContext(ctx, "inventory expiry scan complete",
		"tenants", len(tenants), "emitted", totalEmitted,
	)
	return nil
}

// ---------------------------------------------------------------------------
// ReorderScanJob
// ---------------------------------------------------------------------------

// ReorderScanJob is the periodic job that scans for products with
// total live-batch quantity below their reorder_level.
//
// river:idempotent — same dedup ledger pattern as ExpiryScanJob.
type ReorderScanJob struct{}

// Kind returns the river-stable job kind.
func (ReorderScanJob) Kind() string { return "inventory.reorder_scan" }

// ReorderScanWorker runs [ReorderScanJob].
type ReorderScanWorker struct {
	river.WorkerDefaults[ReorderScanJob]
	repo AlertScanRepo
	log  *slog.Logger
	now  func() time.Time
}

// NewReorderScanWorker wires the worker.
func NewReorderScanWorker(repo AlertScanRepo, log *slog.Logger, now func() time.Time) *ReorderScanWorker {
	if repo == nil {
		panic("jobs: NewReorderScanWorker repo required")
	}
	if log == nil {
		panic("jobs: NewReorderScanWorker log required")
	}
	if now == nil {
		now = time.Now
	}
	return &ReorderScanWorker{repo: repo, log: log, now: now}
}

// Timeout enforces JobTimeout per river canon.
func (*ReorderScanWorker) Timeout(*river.Job[ReorderScanJob]) time.Duration { return JobTimeout }

// Work iterates all tenants, scans for below-reorder products, dedups
// via alert_emissions, and emits ProductBelowReorderLevelV1.
func (w *ReorderScanWorker) Work(ctx context.Context, _ *river.Job[ReorderScanJob]) error {
	now := w.now().UTC()
	today := truncateToDay(now)

	tenants, err := w.repo.ListTenants(ctx)
	if err != nil {
		return fmt.Errorf("reorder scan: list tenants: %w", err)
	}

	totalEmitted := 0
	for _, tid := range tenants {
		products, err := w.repo.ListProductsBelowReorder(ctx, tid, today)
		if err != nil {
			return fmt.Errorf("reorder scan: tenant %s rows: %w", tid, err)
		}
		for _, p := range products {
			evt := integrationevents.ProductBelowReorderLevelV1{
				ProductID:          p.ProductID,
				TenantIDClaim:      p.TenantID,
				SKU:                p.SKU,
				ReorderLevel:       p.ReorderLevel,
				CurrentStockOnHand: p.StockOnHand,
				OccurredAtUTC:      now,
			}
			emitted, err := w.repo.EmitIfNew(ctx, p.TenantID, AlertKindProductBelowReorder, p.ProductID, today, evt)
			if err != nil {
				return fmt.Errorf("reorder scan: emit %s: %w", p.ProductID, err)
			}
			if emitted {
				totalEmitted++
			}
		}
	}
	w.log.InfoContext(ctx, "inventory reorder scan complete",
		"tenants", len(tenants), "emitted", totalEmitted,
	)
	return nil
}

// ---------------------------------------------------------------------------
// Periodic scheduling helpers
// ---------------------------------------------------------------------------

// ExpiryScanInterval is the periodic run interval. Daily per BRD §6.5.
// (v0.2 uses simple interval; v0.3 will wire a cron expression for
// fixed clock-time alignment per the original spec — daily ~02:30 UTC.)
const ExpiryScanInterval = 24 * time.Hour

// ReorderScanInterval is the periodic run interval. Daily per BRD §6.5.
const ReorderScanInterval = 24 * time.Hour

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// truncateToDay returns t rounded down to UTC midnight.
func truncateToDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// daysBetween returns ceiling-days between today and expiry.
func daysBetween(today time.Time, expiry time.Time) int {
	diff := expiry.Sub(today)
	return int(diff.Hours() / 24)
}
