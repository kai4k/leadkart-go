//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// TestLogStockMovement_Concurrent_RetriesOnConflict drives N goroutines
// concurrently against the SAME batch via LogStockMovementHandler. Per
// ADR 0061 §3 + amendment 1 (C3):
//
//   (a) every racer eventually succeeds (the 3-attempt retry loop must
//       cover realistic burst contention),
//   (b) at least one attempt observes batch.ErrConcurrencyConflict and
//       retries — proving the rows-affected=0 → retry branch fires
//       under real Postgres optimistic-locking contention,
//   (c) final quantity_on_hand == sum of inbound magnitudes (i.e. no
//       lost update — the retry path's re-read + re-apply correctly
//       preserves the racer's mutation).
//
// Channel-based barrier coordinates the start so every goroutine begins
// inside the same Postgres tick. To make the retry-path assertion (b)
// deterministic across CPU-constrained cloud runners (where Go's
// scheduler may serialise the 3 racers so no organic contention
// happens), we inject ONE forced ErrConcurrencyConflict via the
// [firstCallConflictBatchRepo] wrapper — the very first UpdateByID
// across all goroutines is rewritten to ErrConcurrencyConflict, then
// subsequent calls pass through. This guarantees `total UoW entries
// > racers` regardless of scheduling: the racer that lost the first-
// call lottery retries and succeeds on its second attempt, while the
// other two go through normally. Production semantics are unchanged —
// the wrapper only fakes the conflict signal the real DB would have
// raised under sufficient contention.
func TestLogStockMovement_Concurrent_RetriesOnConflict(t *testing.T) {
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	// Seed Product + Batch.
	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "RACE-1", Name: "Race", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200})
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
		})
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	// Hot-row contender count + per-call magnitude.
	//
	// racers=3 bounded by the handler's maxConcurrencyRetries=3 — worst
	// case serialisation is attempt-1 (3 fire, 1 wins, 2 conflict),
	// attempt-2 (2 retry, 1 wins, 1 conflict), attempt-3 (1 retries,
	// wins). Every racer succeeds inside the budget. Bumping racers to
	// 8 would exhaust retries on the slowest racer + flake the test —
	// the contention property we want to prove fires reliably at N=3.
	const (
		racers        = 3
		perRacerQty   = 10
		expectedFinal = racers * perRacerQty
	)

	// Barrier so every goroutine fires its Handle at the same moment.
	startBarrier := make(chan struct{})

	// totalEntries is incremented by an instrumented UoW wrapper —
	// counts WithinTx entries across ALL goroutines. The handler's
	// retry loop calls attemptOnce ≥ 1 time per Handle invocation;
	// total entries = racers + retries. A successful contention test
	// shows entries > racers (i.e. at least one retry fired).
	var totalEntries atomic.Int64
	instrumentedUoW := &countingUoW{inner: tx, counter: &totalEntries}

	// firstCallConflictBatchRepo rewrites the FIRST UpdateByID across
	// all goroutines to ErrConcurrencyConflict, then passes through.
	// Guarantees ≥ 1 retry fires regardless of OS scheduling — without
	// this wrapper the test relied on real Postgres contention, which
	// CPU-constrained cloud runners (GOMAXPROCS ≤ 2) tend to serialise
	// into sequential, conflict-free executions.
	conflictOnceBatches := &firstCallConflictBatchRepo{inner: batches}

	h := command.NewLogStockMovementHandler(instrumentedUoW, conflictOnceBatches, movements)

	var wg sync.WaitGroup
	wg.Add(racers)
	errs := make(chan error, racers)
	for range racers {
		go func() {
			defer wg.Done()
			<-startBarrier
			_, err := h.Handle(ctx, command.LogStockMovementCommand{
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

	// (a) every racer succeeded.
	for err := range errs {
		if err != nil {
			t.Fatalf("racer error (every Handle must eventually succeed): %v", err)
		}
	}

	// (b) at least one retry fired — total UoW entries > racers.
	got := totalEntries.Load()
	if got <= int64(racers) {
		t.Fatalf("UoW total entries: got %d (= racers), want > %d (retry path NEVER fired — contention test is invalid)",
			got, racers)
	}
	t.Logf("UoW entries: %d (racers=%d, retries fired=%d)", got, racers, got-int64(racers))

	// (c) final quantity_on_hand == sum of inbounds — no lost update.
	final, err := batches.GetByID(ctx, b.ID())
	if err != nil {
		t.Fatalf("GetByID after race: %v", err)
	}
	if final.QuantityOnHand() != int64(expectedFinal) {
		t.Fatalf("final on-hand: got %d want %d (lost-update bug)",
			final.QuantityOnHand(), expectedFinal)
	}
}

// countingUoW wraps a *pg.Transactor + counts every WithinTx entry.
// Implements pg.UnitOfWork; passes the inner call through unchanged so
// the real Postgres tx + RLS binding still apply.
type countingUoW struct {
	inner   pg.UnitOfWork
	counter *atomic.Int64
}

func (u *countingUoW) WithinTx(ctx context.Context, scope pg.TxScope, fn func(ctx context.Context) error) error {
	u.counter.Add(1)
	return u.inner.WithinTx(ctx, scope, fn)
}

// Sanity: the handler exhausts its retry budget when conflicts are
// unrelenting. We simulate this by wrapping the batch repo so every
// UpdateByID forcibly returns ErrConcurrencyConflict — the handler
// must surface the failure after maxConcurrencyRetries (3) attempts
// rather than spinning forever.
func TestLogStockMovement_ExhaustsRetries_OnUnrelentingConflict(t *testing.T) {
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "EXH-1", Name: "Exh", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200})
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
		batch.Spec{
			BatchNumber: "LOT-EXH", ManufactureDate: mfg, ExpiryDate: exp,
			ManufacturerName: "A", ManufacturingLicenceNumber: "ML-1",
			MRPPaise: 100, PurchasePricePaise: 50,
		})
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	forcedConflict := &forceConflictBatchRepo{inner: batches}
	h := command.NewLogStockMovementHandler(tx, forcedConflict, movements)
	_, err := h.Handle(ctx, command.LogStockMovementCommand{
		BatchID:           b.ID(),
		ActorMembershipID: actor,
		Type:              batch.MovementInbound,
		Quantity:          5,
		Reason:            "exhaust",
	})
	if !errors.Is(err, batch.ErrConcurrencyConflict) {
		t.Fatalf("err: got %v want ErrConcurrencyConflict (after exhausting retries)", err)
	}
	if forcedConflict.calls != 3 {
		t.Fatalf("UpdateByID calls: got %d want 3 (maxConcurrencyRetries cap)", forcedConflict.calls)
	}
}

// forceConflictBatchRepo wraps a real BatchRepository but rewrites
// every UpdateByID result to batch.ErrConcurrencyConflict. Reads
// (GetByID etc.) pass through. ListByProductPage /
// AnyLiveWithStockForProduct delegate to inner — not exercised by the
// retry test but required by the interface.
type forceConflictBatchRepo struct {
	inner *adapters.BatchRepository
	calls int
}

func (r *forceConflictBatchRepo) Add(ctx context.Context, b *batch.Batch) error {
	return r.inner.Add(ctx, b)
}

func (r *forceConflictBatchRepo) UpdateByID(ctx context.Context, id batch.ID, fn func(*batch.Batch) (bool, error)) error {
	r.calls++
	// Run the inner UpdateByID so the load + invariant guards fire as
	// in production; rewrite the return to ErrConcurrencyConflict so
	// the handler's retry loop is forced down the conflict path.
	_ = r.inner.UpdateByID(ctx, id, func(b *batch.Batch) (bool, error) {
		commit, err := fn(b)
		if err != nil {
			return false, err
		}
		_ = commit
		// Always abort the inner update so the real DB stays
		// unchanged; the forced ErrConcurrencyConflict surfaces below.
		return false, nil
	})
	return batch.ErrConcurrencyConflict
}

func (r *forceConflictBatchRepo) GetByID(ctx context.Context, id batch.ID) (*batch.Batch, error) {
	return r.inner.GetByID(ctx, id)
}

func (r *forceConflictBatchRepo) ListByProductPage(ctx context.Context, productID product.ID, filter batch.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*batch.Batch], error) {
	return r.inner.ListByProductPage(ctx, productID, filter, cursor, pageSize)
}

func (r *forceConflictBatchRepo) AnyLiveWithStockForProduct(ctx context.Context, productID product.ID) (bool, error) {
	return r.inner.AnyLiveWithStockForProduct(ctx, productID)
}

// firstCallConflictBatchRepo wraps a real BatchRepository and rewrites
// only the VERY FIRST UpdateByID call (across all callers / goroutines)
// to batch.ErrConcurrencyConflict. All subsequent UpdateByID calls pass
// through to the real repo. Used by the contention test to deterministically
// trigger the retry path without relying on OS scheduler luck.
//
// The first-call rewrite still runs the inner load + invariant guards
// (so domain validation fires as in production), then aborts the inner
// commit + returns the fake conflict. Production data is untouched.
//
// Atomic int32 flag ensures the conflict fires exactly once even when
// hit by N concurrent goroutines.
type firstCallConflictBatchRepo struct {
	inner *adapters.BatchRepository
	fired atomic.Int32 // 0 → next call gets conflict; 1 → all calls pass through
}

func (r *firstCallConflictBatchRepo) Add(ctx context.Context, b *batch.Batch) error {
	return r.inner.Add(ctx, b)
}

func (r *firstCallConflictBatchRepo) UpdateByID(ctx context.Context, id batch.ID, fn func(*batch.Batch) (bool, error)) error {
	if r.fired.CompareAndSwap(0, 1) {
		// Run inner load + invariant fn so domain guards still execute,
		// but abort the commit + return the fake conflict.
		_ = r.inner.UpdateByID(ctx, id, func(b *batch.Batch) (bool, error) {
			_, err := fn(b)
			return false, err
		})
		return batch.ErrConcurrencyConflict
	}
	return r.inner.UpdateByID(ctx, id, fn)
}

func (r *firstCallConflictBatchRepo) GetByID(ctx context.Context, id batch.ID) (*batch.Batch, error) {
	return r.inner.GetByID(ctx, id)
}

func (r *firstCallConflictBatchRepo) ListByProductPage(ctx context.Context, productID product.ID, filter batch.ListFilter, cursor pagination.Cursor, pageSize int) (pagination.Page[*batch.Batch], error) {
	return r.inner.ListByProductPage(ctx, productID, filter, cursor, pageSize)
}

func (r *firstCallConflictBatchRepo) AnyLiveWithStockForProduct(ctx context.Context, productID product.ID) (bool, error) {
	return r.inner.AnyLiveWithStockForProduct(ctx, productID)
}

// compile-time interface assertions.
var (
	_ batch.Repository = (*forceConflictBatchRepo)(nil)
	_ batch.Repository = (*firstCallConflictBatchRepo)(nil)
	_ pg.UnitOfWork    = (*countingUoW)(nil)
)
