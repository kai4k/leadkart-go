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

	"github.com/leadkart/leadkart-go/internal/inventory/app/jobs"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// fixedNow is the deterministic instant the worker tests pin against —
// the Phase A.3 mid-day UTC instant the rest of the inventory test
// suite uses too.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// fakeAlertScanRepo is the in-memory test double for the AlertScanRepo
// contract. Drives both worker tests without spinning Postgres.
type fakeAlertScanRepo struct {
	tenants         []uuid.UUID
	expiringByTenant map[uuid.UUID][]jobs.BatchExpiring
	reorderByTenant  map[uuid.UUID][]jobs.ReorderProduct
	emitted          []emittedAlert
	listTenantsErr   error
	emitErr          error
	// dedupKeys: caller-supplied set of (kind, subjectID) tuples that
	// are PRE-EMITTED today; the EmitIfNew returns (false, nil) for them.
	dedupKeys map[string]struct{}
}

type emittedAlert struct {
	TenantID  uuid.UUID
	Kind      string
	SubjectID uuid.UUID
	Event     integrationevents.Event
}

func newFake() *fakeAlertScanRepo {
	return &fakeAlertScanRepo{
		expiringByTenant: map[uuid.UUID][]jobs.BatchExpiring{},
		reorderByTenant:  map[uuid.UUID][]jobs.ReorderProduct{},
		dedupKeys:        map[string]struct{}{},
	}
}

func (r *fakeAlertScanRepo) ListTenants(_ context.Context) ([]uuid.UUID, error) {
	if r.listTenantsErr != nil {
		return nil, r.listTenantsErr
	}
	return r.tenants, nil
}

func (r *fakeAlertScanRepo) ListBatchesNearExpiry(_ context.Context, tenantID uuid.UUID, _ time.Time) ([]jobs.BatchExpiring, error) {
	return r.expiringByTenant[tenantID], nil
}

func (r *fakeAlertScanRepo) ListProductsBelowReorder(_ context.Context, tenantID uuid.UUID, _ time.Time) ([]jobs.ReorderProduct, error) {
	return r.reorderByTenant[tenantID], nil
}

func (r *fakeAlertScanRepo) EmitIfNew(_ context.Context, tenantID uuid.UUID, kind string, subjectID uuid.UUID, _ time.Time, event integrationevents.Event) (bool, error) {
	if r.emitErr != nil {
		return false, r.emitErr
	}
	key := kind + ":" + subjectID.String()
	if _, dup := r.dedupKeys[key]; dup {
		return false, nil
	}
	r.dedupKeys[key] = struct{}{}
	r.emitted = append(r.emitted, emittedAlert{
		TenantID:  tenantID,
		Kind:      kind,
		SubjectID: subjectID,
		Event:     event,
	})
	return true, nil
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ----- ExpiryScanWorker -----------------------------------------------------

func TestExpiryScanWorker_HappyPath_EmitsForEachBatch(t *testing.T) {
	t.Parallel()
	tenantA := uuid.Must(uuid.NewV7())
	batchA := uuid.Must(uuid.NewV7())
	productA := uuid.Must(uuid.NewV7())
	repo := newFake()
	repo.tenants = []uuid.UUID{tenantA}
	repo.expiringByTenant[tenantA] = []jobs.BatchExpiring{{
		TenantID:      tenantA,
		ProductID:     productA,
		BatchID:       batchA,
		BatchNumber:   "B-001",
		ExpiryDate:    fixedNow.AddDate(0, 0, 30),
		ThresholdDays: 90,
	}}

	w := jobs.NewExpiryScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{Args: jobs.ExpiryScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 1 {
		t.Fatalf("emitted: got %d want 1", len(repo.emitted))
	}
	got := repo.emitted[0]
	if got.Kind != jobs.AlertKindBatchExpiring {
		t.Errorf("kind: got %q want %q", got.Kind, jobs.AlertKindBatchExpiring)
	}
	if got.SubjectID != batchA {
		t.Errorf("subject id: got %s want %s", got.SubjectID, batchA)
	}
	evt, ok := got.Event.(integrationevents.BatchExpiringSoonV1)
	if !ok {
		t.Fatalf("event type: got %T want BatchExpiringSoonV1", got.Event)
	}
	if evt.BatchID != batchA {
		t.Errorf("event batch id: got %s want %s", evt.BatchID, batchA)
	}
	if evt.DaysUntilExpiry != 30 {
		t.Errorf("days until expiry: got %d want 30", evt.DaysUntilExpiry)
	}
}

func TestExpiryScanWorker_Dedup_SecondRunNoOp(t *testing.T) {
	t.Parallel()
	tenantA := uuid.Must(uuid.NewV7())
	batchA := uuid.Must(uuid.NewV7())
	productA := uuid.Must(uuid.NewV7())
	repo := newFake()
	repo.tenants = []uuid.UUID{tenantA}
	repo.expiringByTenant[tenantA] = []jobs.BatchExpiring{{
		TenantID: tenantA, ProductID: productA, BatchID: batchA,
		BatchNumber: "B-001", ExpiryDate: fixedNow.AddDate(0, 0, 10),
		ThresholdDays: 90,
	}}
	// Pre-mark as already emitted today.
	repo.dedupKeys[jobs.AlertKindBatchExpiring+":"+batchA.String()] = struct{}{}

	w := jobs.NewExpiryScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{Args: jobs.ExpiryScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 0 {
		t.Errorf("emitted: got %d want 0 (dedup should skip)", len(repo.emitted))
	}
}

func TestExpiryScanWorker_NoTenants_CleanNoOp(t *testing.T) {
	t.Parallel()
	repo := newFake()
	w := jobs.NewExpiryScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{Args: jobs.ExpiryScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 0 {
		t.Errorf("emitted: got %d want 0", len(repo.emitted))
	}
}

func TestExpiryScanWorker_ListTenantsError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFake()
	sentinel := errors.New("db blew up")
	repo.listTenantsErr = sentinel
	w := jobs.NewExpiryScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	err := w.Work(t.Context(), &river.Job[jobs.ExpiryScanJob]{Args: jobs.ExpiryScanJob{}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err: got %v want sentinel", err)
	}
}

// ----- ReorderScanWorker ----------------------------------------------------

func TestReorderScanWorker_HappyPath_EmitsForEachProduct(t *testing.T) {
	t.Parallel()
	tenantA := uuid.Must(uuid.NewV7())
	productA := uuid.Must(uuid.NewV7())
	repo := newFake()
	repo.tenants = []uuid.UUID{tenantA}
	repo.reorderByTenant[tenantA] = []jobs.ReorderProduct{{
		TenantID: tenantA, ProductID: productA,
		SKU: "AMOX-500", ReorderLevel: 100, StockOnHand: 25,
	}}

	w := jobs.NewReorderScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ReorderScanJob]{Args: jobs.ReorderScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 1 {
		t.Fatalf("emitted: got %d want 1", len(repo.emitted))
	}
	got := repo.emitted[0]
	if got.Kind != jobs.AlertKindProductBelowReorder {
		t.Errorf("kind: got %q want %q", got.Kind, jobs.AlertKindProductBelowReorder)
	}
	evt, ok := got.Event.(integrationevents.ProductBelowReorderLevelV1)
	if !ok {
		t.Fatalf("event type: got %T want ProductBelowReorderLevelV1", got.Event)
	}
	if evt.SKU != "AMOX-500" {
		t.Errorf("sku: got %q want %q", evt.SKU, "AMOX-500")
	}
	if evt.ReorderLevel != 100 || evt.CurrentStockOnHand != 25 {
		t.Errorf("levels: got (reorder=%d, stock=%d) want (100, 25)", evt.ReorderLevel, evt.CurrentStockOnHand)
	}
}

func TestReorderScanWorker_Dedup_SecondRunNoOp(t *testing.T) {
	t.Parallel()
	tenantA := uuid.Must(uuid.NewV7())
	productA := uuid.Must(uuid.NewV7())
	repo := newFake()
	repo.tenants = []uuid.UUID{tenantA}
	repo.reorderByTenant[tenantA] = []jobs.ReorderProduct{{
		TenantID: tenantA, ProductID: productA, SKU: "X-1",
		ReorderLevel: 50, StockOnHand: 5,
	}}
	repo.dedupKeys[jobs.AlertKindProductBelowReorder+":"+productA.String()] = struct{}{}

	w := jobs.NewReorderScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ReorderScanJob]{Args: jobs.ReorderScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 0 {
		t.Errorf("emitted: got %d want 0 (dedup should skip)", len(repo.emitted))
	}
}

func TestReorderScanWorker_NoEligibleProducts_CleanNoOp(t *testing.T) {
	t.Parallel()
	tenantA := uuid.Must(uuid.NewV7())
	repo := newFake()
	repo.tenants = []uuid.UUID{tenantA}
	// No products under reorder for this tenant.

	w := jobs.NewReorderScanWorker(repo, silentLogger(), func() time.Time { return fixedNow })
	if err := w.Work(t.Context(), &river.Job[jobs.ReorderScanJob]{Args: jobs.ReorderScanJob{}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(repo.emitted) != 0 {
		t.Errorf("emitted: got %d want 0", len(repo.emitted))
	}
}
