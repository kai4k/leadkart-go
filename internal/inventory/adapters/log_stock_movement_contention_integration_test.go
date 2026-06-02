//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each other's state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.

package adapters_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// TestLogStockMovement_Concurrent_NoLostUpdate fires N goroutines against
// the same batch and asserts final on-hand equals the sum of all inbound
// quantities. BatchRepository.UpdateByID serializes writers via
// SELECT ... FOR UPDATE; ErrConcurrencyConflict is unreachable in
// production (Postgres §13.3.2 + Stripe ledger + DDIA Ch.7).
func TestLogStockMovement_Concurrent_NoLostUpdate(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "RACE-1", Name: "Race", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
		batch.Spec{
			BatchNumber: "LOT-RACE", ManufactureDate: mfg, ExpiryDate: exp,
			ManufacturerName: "A", ManufacturingLicenceNumber: "ML-1",
			MRPPaise: 100, PurchasePricePaise: 50,
		}, fixedNow)
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	// 8 racers: enough to stress lock acquisition; without DB
	// serialization this produces lost updates on most runners.
	const (
		racers        = 8
		perRacerQty   = 10
		expectedFinal = racers * perRacerQty
	)

	// totalEntries proves no retry path fires: exactly one tx per Handle.
	// A future accidental reintroduction of optimistic-retry would bump this.
	var totalEntries atomic.Int64
	instrumentedUoW := &countingUoW{inner: tx, counter: &totalEntries}

	h := command.NewLogStockMovementHandler(instrumentedUoW, batches, movements, func() time.Time { return fixedNow }, func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) })

	// Channel barrier: all goroutines release simultaneously to maximise
	// lock-acquisition pressure.
	startBarrier := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(racers)
	errs := make(chan error, racers)
	for range racers {
		go func() {
			defer wg.Done()
			<-startBarrier
			_, err := h.Handle(ctx, command.LogStockMovementCommand{
				TenantID:          tid,
				BatchID:           b.ID(),
				ActorMembershipID: actor,
				Type:              batch.MovementInbound,
				Quantity:          perRacerQty,
				Reason:            "race-inbound",
			})
			errs <- err
		}()
	}
	close(startBarrier)
	wg.Wait()
	close(errs)

	// (a) every racer succeeded — no lock timeout or spurious conflict.
	for err := range errs {
		if err != nil {
			t.Fatalf("racer error (every Handle must succeed): %v", err)
		}
	}

	// (b) exactly one tx per Handle — retry path must not fire.
	if got := totalEntries.Load(); got != int64(racers) {
		t.Fatalf("UoW entries: got %d want %d (no retry path expected with FOR UPDATE)", got, racers)
	}

	// (c) final on-hand == sum of inbounds — lost updates surface as final < expected.
	final, err := batches.GetByID(ctx, tid, b.ID())
	if err != nil {
		t.Fatalf("GetByID after race: %v", err)
	}
	if final.QuantityOnHand() != int64(expectedFinal) {
		t.Fatalf("final on-hand: got %d want %d (lost-update — pessimistic lock failed to serialize)",
			final.QuantityOnHand(), expectedFinal)
	}
}

// countingUoW wraps pg.UnitOfWork and counts every WithinTx call.
// All behaviour is delegated to inner so the real Postgres tx and RLS
// binding apply unchanged.
type countingUoW struct {
	inner   pg.UnitOfWork
	counter *atomic.Int64
}

func (u *countingUoW) WithinTx(ctx context.Context, scope pg.TxScope, fn func(ctx context.Context) error) error {
	u.counter.Add(1)
	return u.inner.WithinTx(ctx, scope, fn)
}

// compile-time pg.UnitOfWork satisfaction.
var _ pg.UnitOfWork = (*countingUoW)(nil)
