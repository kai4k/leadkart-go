package command_test

import (
	"context"
	"errors"
	"sync"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// ----- fakeUoW ---------------------------------------------------------------

// fakeUoW is a pass-through UnitOfWork — runs fn directly with the
// supplied ctx, no real Postgres tx. Tests that exercise
// add-batch-handler / log-stock-movement-handler use this to drive the
// retry loop / re-check branches without spinning Postgres.
type fakeUoW struct {
	// runs counts how many times WithinTx was entered (for retry-loop
	// assertions).
	mu   sync.Mutex
	runs int
}

func (u *fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	u.mu.Lock()
	u.runs++
	u.mu.Unlock()
	return fn(ctx)
}

func (u *fakeUoW) Runs() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.runs
}

// ----- fakeProductRepo -------------------------------------------------------

// fakeProductRepo is an in-memory product.Repository for handler tests.
// Round-trips Add/UpdateByID/GetByID; tracks `addCalls` for assertions.
// Records the LAST ListPage filter for spot-checks.
type fakeProductRepo struct {
	mu          sync.Mutex
	products    map[product.ID]*product.Product
	getErr      error // override GetByID return (e.g. force ErrNotFound)
	addErr      error
	updateErr   error
	addCalls    int
	updateCalls int
}

func newFakeProductRepo() *fakeProductRepo {
	return &fakeProductRepo{products: make(map[product.ID]*product.Product)}
}

func (r *fakeProductRepo) Add(_ context.Context, p *product.Product) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addCalls++
	if r.addErr != nil {
		return r.addErr
	}
	for _, existing := range r.products {
		if existing.TenantID() == p.TenantID() && existing.SKU() == p.SKU() && !existing.IsDeleted() {
			return product.ErrSKUTaken
		}
	}
	_ = p.PullEvents()
	r.products[p.ID()] = p
	return nil
}

func (r *fakeProductRepo) UpdateByID(_ context.Context, id product.ID, fn func(*product.Product) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	if r.updateErr != nil {
		return r.updateErr
	}
	p, ok := r.products[id]
	if !ok {
		return product.ErrNotFound
	}
	commit, err := fn(p)
	if err != nil {
		return err
	}
	if commit {
		_ = p.PullEvents()
	}
	return nil
}

func (r *fakeProductRepo) GetByID(_ context.Context, id product.ID) (*product.Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	p, ok := r.products[id]
	if !ok || p.IsDeleted() {
		return nil, product.ErrNotFound
	}
	return p, nil
}

func (r *fakeProductRepo) ListPage(_ context.Context, _ tenant.ID, _ product.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*product.Product], error) {
	// Not used by command handlers; tests in the query package exercise
	// the listing path against a real testcontainers DB. Returning the
	// zero page keeps the interface satisfied without a list fake
	// nobody exercises from the command layer.
	return pagination.Page[*product.Product]{Items: []*product.Product{}}, nil
}

// ----- fakeBatchRepo ---------------------------------------------------------

// fakeBatchRepo is an in-memory batch.Repository. The version field is
// honoured on UpdateByID: callers that re-load via a `loadedVersion`
// override observe a stale value, simulating a concurrent racer.
type fakeBatchRepo struct {
	mu sync.Mutex

	batches   map[batch.ID]*batch.Batch
	addErr    error
	updateErr error

	// conflictsBeforeSuccess: when > 0, UpdateByID rejects the first N
	// commits with batch.ErrConcurrencyConflict before the (N+1)-th
	// commit succeeds. Drives the retry loop test without needing
	// goroutines (kept here for ergonomic single-threaded assertions;
	// the real contention test in adapters/ uses goroutines).
	conflictsBeforeSuccess int

	// anyLiveStock controls the AnyLiveWithStockForProduct return.
	anyLiveStockFor product.ID
	anyLiveStockOn  bool

	addCalls    int
	updateCalls int
}

func newFakeBatchRepo() *fakeBatchRepo {
	return &fakeBatchRepo{batches: make(map[batch.ID]*batch.Batch)}
}

func (r *fakeBatchRepo) Add(_ context.Context, b *batch.Batch) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addCalls++
	if r.addErr != nil {
		return r.addErr
	}
	for _, existing := range r.batches {
		if existing.ProductID() == b.ProductID() && existing.BatchNumber() == b.BatchNumber() && !existing.IsDeleted() {
			return batch.ErrBatchNumberTaken
		}
	}
	_ = b.PullEvents()
	r.batches[b.ID()] = b
	return nil
}

func (r *fakeBatchRepo) UpdateByID(_ context.Context, id batch.ID, fn func(*batch.Batch) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	if r.updateErr != nil {
		return r.updateErr
	}
	b, ok := r.batches[id]
	if !ok {
		return batch.ErrNotFound
	}
	commit, err := fn(b)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	if r.conflictsBeforeSuccess > 0 {
		r.conflictsBeforeSuccess--
		// roll back the in-memory mutation by re-hydrating from snapshot
		// — simulates the optimistic-concurrency UPDATE WHERE version=$
		// being rejected; the application handler's retry path re-reads.
		// For this in-memory fake we just drain events and signal conflict;
		// the caller's retry path re-loads, which here is the same row
		// pre-mutation only if the test specifically re-loads — which the
		// retry path does via fn() called twice.
		_ = b.PullEvents()
		return batch.ErrConcurrencyConflict
	}
	_ = b.PullEvents()
	return nil
}

func (r *fakeBatchRepo) GetByID(_ context.Context, id batch.ID) (*batch.Batch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.batches[id]
	if !ok || b.IsDeleted() {
		return nil, batch.ErrNotFound
	}
	return b, nil
}

func (r *fakeBatchRepo) ListByProductPage(_ context.Context, _ product.ID, _ batch.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*batch.Batch], error) {
	return pagination.Page[*batch.Batch]{Items: []*batch.Batch{}}, nil
}

func (r *fakeBatchRepo) AnyLiveWithStockForProduct(_ context.Context, productID product.ID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.anyLiveStockFor == productID {
		return r.anyLiveStockOn, nil
	}
	return false, nil
}

// ----- fakeMovementRepo ------------------------------------------------------

type fakeMovementRepo struct {
	mu        sync.Mutex
	movements map[stockmovement.ID]*stockmovement.Movement
	addErr    error
	addCalls  int
}

func newFakeMovementRepo() *fakeMovementRepo {
	return &fakeMovementRepo{movements: make(map[stockmovement.ID]*stockmovement.Movement)}
}

func (r *fakeMovementRepo) Add(_ context.Context, m *stockmovement.Movement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addCalls++
	if r.addErr != nil {
		return r.addErr
	}
	r.movements[m.ID()] = m
	return nil
}

func (r *fakeMovementRepo) GetByID(_ context.Context, id stockmovement.ID) (*stockmovement.Movement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.movements[id]
	if !ok {
		return nil, stockmovement.ErrNotFound
	}
	return m, nil
}

func (r *fakeMovementRepo) ListByBatchPage(_ context.Context, _ batch.ID, _ stockmovement.PageRequest) (pagination.Page[*stockmovement.Movement], error) {
	return pagination.Page[*stockmovement.Movement]{Items: []*stockmovement.Movement{}}, nil
}

// ----- compile-time interface checks -----------------------------------------

var (
	_ product.Repository       = (*fakeProductRepo)(nil)
	_ batch.Repository         = (*fakeBatchRepo)(nil)
	_ stockmovement.Repository = (*fakeMovementRepo)(nil)
	_ pg.UnitOfWork            = (*fakeUoW)(nil)
)

// errSentinel is a generic non-typed error for "any non-domain error".
var errSentinel = errors.New("sentinel")
