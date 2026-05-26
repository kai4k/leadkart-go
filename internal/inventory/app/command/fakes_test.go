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

// fakeUoW is a pass-through UnitOfWork — runs fn directly with the
// supplied ctx, no real Postgres tx. Tests that exercise
// add-batch-handler / log-stock-movement-handler use this to drive the
// retry loop / re-check branches without spinning Postgres.
type fakeUoW struct {
	// runs counts how many times WithinTx was entered (for retry-loop
	// assertions). Sync primitives are permitted here because fakeUoW
	// lives in app-layer _test.go (not under domain/) and the test-side
	// arch gate exempts test files from the no-sync-in-domain rule.
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

// The per-aggregate fakes live in the canonical <aggregate>test/
// directories per TDL Wild Workouts canon — co-located with the
// aggregate they fake. The newFakeXRepo helpers below are one-line
// aliases so existing tests don't need rewriting at the call sites.

func newFakeProductRepo() *producttest.FakeRepository       { return producttest.NewFakeRepository() }
func newFakeBatchRepo() *batchtest.FakeRepository           { return batchtest.NewFakeRepository() }
func newFakeMovementRepo() *stockmovementtest.FakeRepository { return stockmovementtest.NewFakeRepository() }

// ----- compile-time interface checks -----------------------------------------

var _ pg.UnitOfWork = (*fakeUoW)(nil)

// errSentinel is a generic non-typed error for "any non-domain error".
var errSentinel = errors.New("sentinel")
