package command_test

import (
	"context"
	"errors"
	"sync"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch/batchtest"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product/producttest"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement/stockmovementtest"
)

// ----- fakeUoW ---------------------------------------------------------------

// fakeUoW is a pass-through UnitOfWork: runs fn directly, no real Postgres tx.
type fakeUoW struct {
	// runs counts WithinTx entries (for retry-loop assertions). sync is
	// allowed here — this is app-layer _test.go, exempt from the
	// no-sync-in-domain arch gate.
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

// ----- per-aggregate fake constructors --------------------------------------

// Per-aggregate fakes live in the canonical <aggregate>test/ dirs (TDL Wild
// Workouts canon). These helpers are aliases so call sites stay unchanged.

func newFakeProductRepo() *producttest.FakeRepository { return producttest.NewFakeRepository() }
func newFakeBatchRepo() *batchtest.FakeRepository     { return batchtest.NewFakeRepository() }
func newFakeMovementRepo() *stockmovementtest.FakeRepository {
	return stockmovementtest.NewFakeRepository()
}

// ----- compile-time interface checks -----------------------------------------

var _ pg.UnitOfWork = (*fakeUoW)(nil)

// errSentinel stands in for any non-domain error.
var errSentinel = errors.New("sentinel")
