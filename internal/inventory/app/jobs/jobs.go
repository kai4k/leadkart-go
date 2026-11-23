// Package jobs holds Inventory module river background workers per
// ADR 0010 + BRD §6.5 + ADR 0070. Two jobs:
//
//   - ExpiryScanJob: for every batch holding stock, raises BatchExpiringSoonV1 once
//     per warning TIER crossed (the company's expiry.warning_days plus the product
//     own threshold), counting to the LAST SELLABLE DAY, and BatchExpiredOnHandV1
//     once per batch whose printed expiry has passed with units not set apart.
//   - ReorderScanJob: raises ProductBelowReorderLevelV1 for products whose SELLABLE
//     stock is below the reorder level, once per business day.
//
// The day, the stop-before months and the tiers are the company's, read through
// the Inventory replica of the policy ([sellability.Resolver]); the rule itself
// is common/expiry. The dedup ledger is `inventory.alert_emissions`, keyed by
// (tenant, kind, subject, emission_key): re-running within a tier or a day is a
// no-op, and river retry on failure is safe. The events reach the outbox in the
// same transaction as the ledger row, and Notifications delivers them.
//
// The expiry scan also runs for ONE tenant when that tenant policy changes
// ([Scanner.ScanTenant]), so a tighter rule raises its alerts at once.
//
// Boundary discipline (ADR 0047): the workers depend on the [AlertScanRepo]
// interface declared here (consumer-side); the concrete pgx-backed
// implementation lives in `internal/inventory/adapters/`.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// JobTimeout is the per-job cap enforced via river's Timeout hook.
const JobTimeout = 10 * time.Minute

// Alert kinds (the dedup ledger and the CHECK in the migration).
const (
	AlertKindBatchExpiring       = "batch_expiring"
	AlertKindProductBelowReorder = "product_below_reorder"
	AlertKindExpiredOnHand       = "expired_on_hand"
)

// ExpiryCandidate is a live batch that still holds units not set apart, with its product.
type ExpiryCandidate struct {
	TenantID      uuid.UUID
	ProductID     uuid.UUID
	BatchID       uuid.UUID
	BatchNumber   string
	ProductSKU    string
	ProductName   string
	ExpiryDate    time.Time
	OnHand        int64
	Reserved      int64
	Quarantined   int64
	ThresholdDays int
}

// ReorderProduct is a product whose sellable stock is below its reorder level.
type ReorderProduct struct {
	TenantID     uuid.UUID
	ProductID    uuid.UUID
	SKU          string
	ReorderLevel int
	StockOnHand  int64
}

// AlertScanRepo is the consumer-side port of the scans.
type AlertScanRepo interface {
	// ListTenants lists the tenants with a live product (the one cross-tenant read).
	ListTenants(ctx context.Context) ([]uuid.UUID, error)
	// ListBatchesForExpiryScan lists the tenant live batches holding stock not set apart.
	ListBatchesForExpiryScan(ctx context.Context, tenantID uuid.UUID) ([]ExpiryCandidate, error)
	// ListProductsBelowReorder lists products whose sellable on-hand (expiry >= minSellable) is below the reorder level.
	ListProductsBelowReorder(ctx context.Context, tenantID uuid.UUID, minSellable time.Time) ([]ReorderProduct, error)
	// EmitIfNew writes the ledger row (tenant, kind, subject, key) and the event
	// to the outbox in ONE transaction, and reports whether it was new.
	EmitIfNew(ctx context.Context, tenantID uuid.UUID, kind string, subjectID uuid.UUID, emissionKey string, event integrationevents.Event) (bool, error)
}

// TierFor returns the warning tier a batch with daysLeft days to its last
// sellable day is in: the smallest tier that is >= daysLeft, or 0 once the day
// has passed. ok is false when the batch is further out than every tier.
func TierFor(daysLeft int, tiers []int) (tier int, ok bool) {
	if daysLeft < 0 {
		return 0, true
	}
	best := -1
	for _, t := range tiers {
		if t >= daysLeft && (best < 0 || t < best) {
			best = t
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}

// tiersFor merges the company warning tiers with the product own threshold.
func tiersFor(w sellability.Window, productThreshold int) []int {
	out := slices.Clone(w.WarningDays)
	if productThreshold > 0 && !slices.Contains(out, productThreshold) {
		out = append(out, productThreshold)
	}
	return out
}

// Scanner runs the scans for one tenant. The workers and the policy-changed
// subscriber share it.
type Scanner struct {
	repo   AlertScanRepo
	window sellability.Resolver
	log    *slog.Logger
	now    func() time.Time
}

// NewScanner wires the scanner. now nil means time.Now.
func NewScanner(repo AlertScanRepo, window sellability.Resolver, log *slog.Logger, now func() time.Time) *Scanner {
	if repo == nil || log == nil {
		panic("jobs: NewScanner repo and log required")
	}
	if now == nil {
		now = time.Now
	}
	return &Scanner{repo: repo, window: window, log: log, now: now}
}

// ScanExpiry raises the expiry alerts of one tenant and returns how many were new.
func (s *Scanner) ScanExpiry(ctx context.Context, tid uuid.UUID) (int, error) {
	w, err := s.window.Resolve(ctx, tenant.ID(tid.String()))
	if err != nil {
		return 0, fmt.Errorf("expiry scan: tenant %s window: %w", tid, err)
	}
	cands, err := s.repo.ListBatchesForExpiryScan(ctx, tid)
	if err != nil {
		return 0, fmt.Errorf("expiry scan: tenant %s rows: %w", tid, err)
	}
	at := s.now().UTC()
	emitted := 0
	for _, c := range cands {
		n, err := s.scanBatch(ctx, w, c, at)
		if err != nil {
			return emitted, err
		}
		emitted += n
	}
	return emitted, nil
}

func (s *Scanner) scanBatch(ctx context.Context, w sellability.Window, c ExpiryCandidate, at time.Time) (int, error) {
	emitted := 0
	// Units past the PRINTED expiry that are not set apart: the law requires them
	// stored apart and marked "Not for sale". Once per batch.
	if w.IsPastPrintedExpiry(c.ExpiryDate) {
		evt := integrationevents.BatchExpiredOnHandV1{
			BatchID: c.BatchID, ProductID: c.ProductID, TenantIDClaim: c.TenantID,
			BatchNumber: c.BatchNumber, ProductSKU: c.ProductSKU, ProductName: c.ProductName,
			ExpiryDate: c.ExpiryDate, Quantity: c.OnHand - c.Quarantined, OccurredAtUTC: at,
		}
		ok, err := s.repo.EmitIfNew(ctx, c.TenantID, AlertKindExpiredOnHand, c.BatchID, "once", evt)
		if err != nil {
			return emitted, fmt.Errorf("expiry scan: emit expired %s: %w", c.BatchID, err)
		}
		if ok {
			emitted++
		}
		return emitted, nil
	}
	daysLeft := w.DaysLeft(c.ExpiryDate)
	tier, due := TierFor(daysLeft, tiersFor(w, c.ThresholdDays))
	if !due {
		return emitted, nil
	}
	evt := integrationevents.BatchExpiringSoonV1{
		BatchID: c.BatchID, ProductID: c.ProductID, TenantIDClaim: c.TenantID,
		BatchNumber: c.BatchNumber, ProductSKU: c.ProductSKU, ProductName: c.ProductName,
		ExpiryDate: c.ExpiryDate, LastSellableDay: w.LastSellableDay(c.ExpiryDate),
		DaysUntilExpiry: daysLeft, TierDays: tier,
		FreeQuantity: c.OnHand - c.Reserved - c.Quarantined, OccurredAtUTC: at,
	}
	ok, err := s.repo.EmitIfNew(ctx, c.TenantID, AlertKindBatchExpiring, c.BatchID, "tier:"+strconv.Itoa(tier), evt)
	if err != nil {
		return emitted, fmt.Errorf("expiry scan: emit %s: %w", c.BatchID, err)
	}
	if ok {
		emitted++
	}
	return emitted, nil
}

// ScanReorder raises the reorder alerts of one tenant, once per business day.
func (s *Scanner) ScanReorder(ctx context.Context, tid uuid.UUID) (int, error) {
	w, err := s.window.Resolve(ctx, tenant.ID(tid.String()))
	if err != nil {
		return 0, fmt.Errorf("reorder scan: tenant %s window: %w", tid, err)
	}
	products, err := s.repo.ListProductsBelowReorder(ctx, tid, w.MinSellable)
	if err != nil {
		return 0, fmt.Errorf("reorder scan: tenant %s rows: %w", tid, err)
	}
	at := s.now().UTC()
	emitted := 0
	for _, p := range products {
		evt := integrationevents.ProductBelowReorderLevelV1{
			ProductID: p.ProductID, TenantIDClaim: p.TenantID, SKU: p.SKU,
			ReorderLevel: p.ReorderLevel, CurrentStockOnHand: p.StockOnHand, OccurredAtUTC: at,
		}
		ok, err := s.repo.EmitIfNew(ctx, p.TenantID, AlertKindProductBelowReorder, p.ProductID, "day:"+w.Today.Format(time.DateOnly), evt)
		if err != nil {
			return emitted, fmt.Errorf("reorder scan: emit %s: %w", p.ProductID, err)
		}
		if ok {
			emitted++
		}
	}
	return emitted, nil
}

// ----- ExpiryScanJob ----------------------------------------------------------

// ExpiryScanJob is the periodic job that raises the expiry alerts of every tenant.
//
// river:idempotent — the dedup ledger makes a re-run within a tier a no-op.
type ExpiryScanJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (ExpiryScanJob) Kind() string { return "inventory.expiry_scan" }

// ExpiryScanWorker runs [ExpiryScanJob].
type ExpiryScanWorker struct {
	river.WorkerDefaults[ExpiryScanJob]
	repo    AlertScanRepo
	scanner *Scanner
	log     *slog.Logger
}

// NewExpiryScanWorker wires the worker.
func NewExpiryScanWorker(repo AlertScanRepo, window sellability.Resolver, log *slog.Logger, now func() time.Time) *ExpiryScanWorker {
	return &ExpiryScanWorker{repo: repo, scanner: NewScanner(repo, window, log, now), log: log}
}

// Timeout enforces JobTimeout per river canon.
func (*ExpiryScanWorker) Timeout(*river.Job[ExpiryScanJob]) time.Duration { return JobTimeout }

// Work scans every tenant. One tenant failing is logged and the others still run;
// the job then fails so river retries (the ledger makes the retry cheap).
func (w *ExpiryScanWorker) Work(ctx context.Context, _ *river.Job[ExpiryScanJob]) error {
	return sweep(ctx, w.repo, w.log, "expiry", w.scanner.ScanExpiry)
}

// ----- ReorderScanJob ---------------------------------------------------------

// ReorderScanJob is the periodic job that raises the reorder alerts of every tenant.
//
// river:idempotent — same dedup ledger pattern as ExpiryScanJob.
type ReorderScanJob struct{}

// Kind returns the river-stable job kind. Never rename.
func (ReorderScanJob) Kind() string { return "inventory.reorder_scan" }

// ReorderScanWorker runs [ReorderScanJob].
type ReorderScanWorker struct {
	river.WorkerDefaults[ReorderScanJob]
	repo    AlertScanRepo
	scanner *Scanner
	log     *slog.Logger
}

// NewReorderScanWorker wires the worker.
func NewReorderScanWorker(repo AlertScanRepo, window sellability.Resolver, log *slog.Logger, now func() time.Time) *ReorderScanWorker {
	return &ReorderScanWorker{repo: repo, scanner: NewScanner(repo, window, log, now), log: log}
}

// Timeout enforces JobTimeout per river canon.
func (*ReorderScanWorker) Timeout(*river.Job[ReorderScanJob]) time.Duration { return JobTimeout }

// Work scans every tenant.
func (w *ReorderScanWorker) Work(ctx context.Context, _ *river.Job[ReorderScanJob]) error {
	return sweep(ctx, w.repo, w.log, "reorder", w.scanner.ScanReorder)
}

func sweep(ctx context.Context, repo AlertScanRepo, log *slog.Logger, what string, scan func(context.Context, uuid.UUID) (int, error)) error {
	tenants, err := repo.ListTenants(ctx)
	if err != nil {
		return fmt.Errorf("%s scan: list tenants: %w", what, err)
	}
	total, failed := 0, 0
	for _, tid := range tenants {
		n, err := scan(ctx, tid)
		total += n
		if err != nil {
			failed++
			log.ErrorContext(ctx, "inventory "+what+" scan failed for a tenant", "tenant_id", tid.String(), "err", err)
		}
	}
	log.InfoContext(ctx, "inventory "+what+" scan complete", "tenants", len(tenants), "emitted", total, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%s scan: %d of %d tenants failed", what, failed, len(tenants))
	}
	return nil
}

// ----- Schedule ---------------------------------------------------------------

// ScanSchedule runs a scan every day at 00:30 in the market default business-day
// offset (the offset comes from the embedded market data, never a constant here).
// A company on another offset still gets its own day: each tenant is scanned
// against ITS business day, and the ledger makes an extra run harmless.
type ScanSchedule struct{ OffsetMinutes int }

// DefaultScanSchedule is 00:30 at the default policy offset.
func DefaultScanSchedule() ScanSchedule {
	return ScanSchedule{OffsetMinutes: commercepolicy.Default().Expiry.BusinessDayUTCOffsetMinutes}
}

// Next returns the first 00:30 (at the offset) strictly after t.
func (s ScanSchedule) Next(t time.Time) time.Time {
	off := time.Duration(s.OffsetMinutes) * time.Minute
	local := t.UTC().Add(off)
	next := time.Date(local.Year(), local.Month(), local.Day(), 0, 30, 0, 0, time.UTC)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Add(-off)
}

// PeriodicOpts runs the scans when the worker starts too: river keeps no
// schedule across restarts, and the ledger makes it idempotent.
func PeriodicOpts() *river.PeriodicJobOpts { return &river.PeriodicJobOpts{RunOnStart: true} }

// ExpiryPeriodicJob is the registration of the expiry scan.
func ExpiryPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(DefaultScanSchedule(),
		func() (river.JobArgs, *river.InsertOpts) { return ExpiryScanJob{}, nil }, PeriodicOpts())
}

// ReorderPeriodicJob is the registration of the reorder scan.
func ReorderPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(DefaultScanSchedule(),
		func() (river.JobArgs, *river.InsertOpts) { return ReorderScanJob{}, nil }, PeriodicOpts())
}
