package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// fixedNow is 24 May 2026 12:00 UTC (17:30 IST, the same business day). The
// default policy stops sales one month before the printed month and warns at
// 180, 90 and 30 days before the LAST SELLABLE day.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return fixedNow }

type emittedAlert struct {
	TenantID  uuid.UUID
	Kind      string
	SubjectID uuid.UUID
	Key       string
	Event     integrationevents.Event
}

// fakeAlertScanRepo is the in-memory AlertScanRepo: a real dedup ledger keyed
// (tenant, kind, subject, emission_key) like the table.
type fakeAlertScanRepo struct {
	tenants        []uuid.UUID
	candidates     map[uuid.UUID][]jobs.ExpiryCandidate
	reorder        map[uuid.UUID][]jobs.ReorderProduct
	emitted        []emittedAlert
	ledger         map[string]bool
	listTenantsErr error
	failTenant     uuid.UUID
}

func newFake() *fakeAlertScanRepo {
	return &fakeAlertScanRepo{
		candidates: map[uuid.UUID][]jobs.ExpiryCandidate{},
		reorder:    map[uuid.UUID][]jobs.ReorderProduct{},
		ledger:     map[string]bool{},
	}
}

func (r *fakeAlertScanRepo) ListTenants(context.Context) ([]uuid.UUID, error) {
	return r.tenants, r.listTenantsErr
}

func (r *fakeAlertScanRepo) ListBatchesForExpiryScan(_ context.Context, tid uuid.UUID) ([]jobs.ExpiryCandidate, error) {
	if tid == r.failTenant {
		return nil, errors.New("tenant read failed")
	}
	return r.candidates[tid], nil
}

func (r *fakeAlertScanRepo) ListProductsBelowReorder(_ context.Context, tid uuid.UUID, _ time.Time) ([]jobs.ReorderProduct, error) {
	return r.reorder[tid], nil
}

func (r *fakeAlertScanRepo) EmitIfNew(_ context.Context, tid uuid.UUID, kind string, subject uuid.UUID, key string, evt integrationevents.Event) (bool, error) {
	k := tid.String() + "|" + kind + "|" + subject.String() + "|" + key
	if r.ledger[k] {
		return false, nil
	}
	r.ledger[k] = true
	r.emitted = append(r.emitted, emittedAlert{TenantID: tid, Kind: kind, SubjectID: subject, Key: key, Event: evt})
	return true, nil
}

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func candidate(tid uuid.UUID, expiry time.Time, onHand, reserved, quarantined int64, threshold int) jobs.ExpiryCandidate {
	return jobs.ExpiryCandidate{
		TenantID: tid, ProductID: uuid.New(), BatchID: uuid.New(), BatchNumber: "B-1", ProductSKU: "SKU-1", ProductName: "Drug",
		ExpiryDate: expiry, OnHand: onHand, Reserved: reserved, Quarantined: quarantined, ThresholdDays: threshold,
	}
}

func TestTierFor(t *testing.T) {
	t.Parallel()
	tiers := []int{180, 90, 30}
	tests := []struct {
		days   int
		want   int
		wantOK bool
	}{
		{400, 0, false}, {181, 0, false}, {180, 180, true}, {100, 180, true}, {91, 180, true}, {90, 90, true},
		{31, 90, true}, {30, 30, true}, {0, 30, true}, {-1, 0, true}, {-40, 0, true},
	}
	for _, tt := range tests {
		got, ok := jobs.TierFor(tt.days, tiers)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("TierFor(%d) = %d,%v want %d,%v", tt.days, got, ok, tt.want, tt.wantOK)
		}
	}
}

// EXP 08/2026 with the default stop of one month: the last sellable day is 31 Jul,
// 68 days from 24 May: inside the 90-day tier. The alert is raised ONCE for that
// tier, however many days the scan runs, and again only when the 30-day tier is crossed.
func TestExpiryScan_OneAlertPerTierCrossedNeverPerDay(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	repo.candidates[tid] = []jobs.ExpiryCandidate{candidate(tid, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 100, 20, 0, 0)}

	run := func(now time.Time) {
		t.Helper()
		w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(now), silentLogger(), func() time.Time { return now })
		if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
			t.Fatalf("Work: %v", err)
		}
	}
	run(fixedNow)
	if len(repo.emitted) != 1 {
		t.Fatalf("first scan emitted %d, want 1", len(repo.emitted))
	}
	evt, ok := repo.emitted[0].Event.(integrationevents.BatchExpiringSoonV1)
	if !ok || evt.TierDays != 90 || evt.DaysUntilExpiry != 68 || evt.FreeQuantity != 80 ||
		evt.LastSellableDay.Format(time.DateOnly) != "2026-07-31" || repo.emitted[0].Key != "tier:90:2026-07-31" {
		t.Fatalf("event = %+v key %q", evt, repo.emitted[0].Key)
	}
	for d := 1; d <= 20; d++ { // every following day inside the tier: nothing new
		run(fixedNow.AddDate(0, 0, d))
	}
	if len(repo.emitted) != 1 {
		t.Fatalf("a daily repeat: %d alerts after 20 more scans, want 1", len(repo.emitted))
	}
	run(time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)) // 26 days left: the 30-day tier
	if len(repo.emitted) != 2 || repo.emitted[1].Key != "tier:30:2026-07-31" {
		t.Fatalf("the next tier: %d alerts, last key %q", len(repo.emitted), repo.emitted[len(repo.emitted)-1].Key)
	}
}

func TestExpiryScan_TheProductThresholdIsAnExtraTierAndFarBatchesAreQuiet(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	far := candidate(tid, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)
	withThreshold := candidate(tid, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 200) // last sellable day 30 Nov: 190 days
	repo.candidates[tid] = []jobs.ExpiryCandidate{far, withThreshold}
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
		t.Fatal(err)
	}
	if len(repo.emitted) != 1 || repo.emitted[0].SubjectID != withThreshold.BatchID || repo.emitted[0].Key != "tier:200:2026-11-30" {
		t.Fatalf("emitted %+v", repo.emitted)
	}
}

// Units past the PRINTED expiry that are not set apart raise one expired_on_hand
// alert per batch (Rule 65(17): they must be stored apart and marked Not for sale).
func TestExpiryScan_ExpiredStockNotSetApartRaisesOneAlertPerBatch(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	expired := candidate(tid, time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), 30, 0, 10, 0) // 20 not set apart
	repo.candidates[tid] = []jobs.ExpiryCandidate{expired}
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	for range 3 {
		if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.emitted) != 1 {
		t.Fatalf("emitted %d, want 1", len(repo.emitted))
	}
	evt, ok := repo.emitted[0].Event.(integrationevents.BatchExpiredOnHandV1)
	if !ok || evt.Quantity != 20 || repo.emitted[0].Kind != jobs.AlertKindExpiredOnHand {
		t.Fatalf("event %+v", repo.emitted[0])
	}
}

// A batch whose printed month is the current one is past the company cut-off but
// not past its printed expiry: it raises the tier-0 expiry alert, not expired_on_hand.
func TestExpiryScan_PastTheCutoffButInsideThePrintedMonthIsTierZero(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	repo.candidates[tid] = []jobs.ExpiryCandidate{candidate(tid, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)}
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
		t.Fatal(err)
	}
	evt, ok := repo.emitted[0].Event.(integrationevents.BatchExpiringSoonV1)
	if len(repo.emitted) != 1 || !ok || evt.TierDays != 0 || evt.DaysUntilExpiry >= 0 {
		t.Fatalf("emitted %+v", repo.emitted)
	}
}

// The tiers are the company own setting.
func TestExpiryScan_UsesTheCompanyWarningDays(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	repo.candidates[tid] = []jobs.ExpiryCandidate{candidate(tid, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)} // 68 days
	p := commercepolicy.Default()
	p.Expiry.WarningDays = []int{60, 10}
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.ResolverWith(p, fixedNow), silentLogger(), clock)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
		t.Fatal(err)
	}
	if len(repo.emitted) != 0 {
		t.Fatalf("68 days is outside the company 60-day tier, got %+v", repo.emitted)
	}
}

func TestExpiryScan_OneTenantFailingDoesNotStopTheOthersButFailsTheJob(t *testing.T) {
	t.Parallel()
	good, bad := uuid.New(), uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{bad, good}
	repo.failTenant = bad
	repo.candidates[good] = []jobs.ExpiryCandidate{candidate(good, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)}
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err == nil {
		t.Fatal("a failed tenant must fail the job so river retries it")
	}
	if len(repo.emitted) != 1 {
		t.Fatalf("the healthy tenant was skipped: %d", len(repo.emitted))
	}
}

func TestExpiryScan_ListTenantsFailure(t *testing.T) {
	t.Parallel()
	repo := newFake()
	repo.listTenantsErr = errors.New("db down")
	w := jobs.NewExpiryScanWorker(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err == nil {
		t.Fatal("a listing failure must fail the job")
	}
}

func TestReorderScan_OneAlertPerBusinessDay(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	repo.reorder[tid] = []jobs.ReorderProduct{{TenantID: tid, ProductID: uuid.New(), SKU: "AMOX", ReorderLevel: 100, StockOnHand: 25}}
	run := func(now time.Time) {
		w := jobs.NewReorderScanWorker(repo, sellabilitytest.Resolver(now), silentLogger(), func() time.Time { return now })
		if err := w.Work(t.Context(), &river.Job[jobs.ReorderScanJob]{}); err != nil {
			t.Fatal(err)
		}
	}
	run(fixedNow)
	run(fixedNow.Add(time.Hour))
	if len(repo.emitted) != 1 || repo.emitted[0].Key != "day:2026-05-24" {
		t.Fatalf("same business day: %+v", repo.emitted)
	}
	run(fixedNow.AddDate(0, 0, 1))
	if len(repo.emitted) != 2 || repo.emitted[1].Key != "day:2026-05-25" {
		t.Fatalf("the next business day: %+v", repo.emitted)
	}
}

func TestJobKindsAreStable(t *testing.T) {
	t.Parallel()
	if (jobs.ExpiryScanJob{}).Kind() != "inventory.expiry_scan" || (jobs.ReorderScanJob{}).Kind() != "inventory.reorder_scan" {
		t.Fatal("river kinds are stable names")
	}
}

// The scans fire at 00:30 in the market default offset (330 minutes), every day,
// and when the worker starts.
func TestScanSchedule_FiresAt0030InTheDefaultOffsetEveryDay(t *testing.T) {
	t.Parallel()
	s := jobs.DefaultScanSchedule()
	if s.OffsetMinutes != 330 {
		t.Fatalf("offset = %d, want the market default 330", s.OffsetMinutes)
	}
	// 00:30 IST = 19:00 UTC the day before.
	got := s.Next(time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)) // 08:30 IST on the 23rd
	if want := time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got, want)
	}
	got = s.Next(time.Date(2026, 11, 23, 19, 0, 0, 0, time.UTC)) // exactly at 00:30 IST: the following day
	if want := time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("next after the run = %s, want %s", got, want)
	}
	if !jobs.PeriodicOpts().RunOnStart || jobs.ExpiryPeriodicJob() == nil || jobs.ReorderPeriodicJob() == nil {
		t.Fatal("the scans run when the worker starts")
	}
}

// Review S2: the dedup key carries the last sellable day, so a tier already raised for
// one rule is raised again when the rule changes the day: after a company tightens and
// then loosens its stop, the real warning for the restored day still fires.
func TestExpiryScan_ARuleChangeThatMovesTheLastSellableDayRaisesTheTierAgain(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{tid}
	repo.candidates[tid] = []jobs.ExpiryCandidate{candidate(tid, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)}
	run := func(stop int, now time.Time) {
		t.Helper()
		p := commercepolicy.Default()
		p.Expiry.StopMonthsBefore = stop
		w := jobs.NewExpiryScanWorker(repo, sellabilitytest.ResolverWith(p, now), silentLogger(), func() time.Time { return now })
		if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{}); err != nil {
			t.Fatal(err)
		}
	}
	run(1, fixedNow) // last sellable day 31 Jul, 68 days: tier 90
	run(2, fixedNow) // tightened: 30 Jun, 37 days: tier 90 again, for another day
	if len(repo.emitted) != 2 {
		t.Fatalf("after tightening: %d alerts, want 2", len(repo.emitted))
	}
	run(1, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)) // loosened, 26 days before 31 Jul: tier 30 for 31 Jul
	if len(repo.emitted) != 3 || repo.emitted[2].Key != "tier:30:2026-07-31" {
		t.Fatalf("after loosening the real warning must fire: %d alerts, last %q", len(repo.emitted), repo.emitted[len(repo.emitted)-1].Key)
	}
}

// Review S1: the rescan job scans exactly the tenant it names.
func TestExpiryRescanWorker_ScansTheNamedTenantOnly(t *testing.T) {
	t.Parallel()
	a, b := uuid.New(), uuid.New()
	repo := newFake()
	repo.tenants = []uuid.UUID{a, b}
	repo.candidates[a] = []jobs.ExpiryCandidate{candidate(a, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)}
	repo.candidates[b] = []jobs.ExpiryCandidate{candidate(b, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), 10, 0, 0, 0)}
	scanner := jobs.NewScanner(repo, sellabilitytest.Resolver(fixedNow), silentLogger(), clock)
	w := jobs.NewExpiryRescanWorker(scanner)
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryRescanJob]{Args: jobs.ExpiryRescanJob{TenantID: a.String()}}); err != nil {
		t.Fatal(err)
	}
	if len(repo.emitted) != 1 || repo.emitted[0].TenantID != a {
		t.Fatalf("emitted %+v", repo.emitted)
	}
	if (jobs.ExpiryRescanJob{}).Kind() != "inventory.expiry_rescan" {
		t.Fatal("river kind is a stable name")
	}
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryRescanJob]{Args: jobs.ExpiryRescanJob{TenantID: "nope"}}); err == nil {
		t.Fatal("a malformed tenant id must be refused (cancelled, not retried)")
	}
}
