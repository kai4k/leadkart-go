//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; the racers run
// millisecond row-locked UPDATEs, total wall time well under a second.

// arch-test:parallel-safe — concurrency is INTERNAL to the test (goroutines
// against one fresh tenant); the test itself stays serial like every other
// integration test in this package.

package adapters_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// TestInvoiceNumberAllocator_ConcurrentAllocationsAreUniqueAndGapless races N
// goroutines × M allocations against ONE (tenant, fy, kind) counter — the
// invariant gapless numbering depends on: the sequence row's row lock
// serialises increments, so the N*M results are exactly 1..N*M with no
// duplicate and no gap. Mirrors the inventory stock-contention precedent
// (log_stock_movement_contention_integration_test.go).
func TestInvoiceNumberAllocator_ConcurrentAllocationsAreUniqueAndGapless(t *testing.T) {
	const (
		racers     = 8
		perRacer   = 5
		totalAlloc = racers * perRacer
	)
	tx := pg.NewTransactor(ordersPool(t))
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	tid := tenant.ID(uuid.NewString())
	fy := invoicenumber.FinancialYear("2026-27")

	results := make(chan int64, totalAlloc)
	errs := make(chan error, totalAlloc)
	testCtx := t.Context()
	var wg sync.WaitGroup
	for r := 0; r < racers; r++ {
		wg.Go(func() {
			for i := 0; i < perRacer; i++ {
				ctx := tenancy.WithID(testCtx, tenancy.ID(tid.String()))
				err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
					n, aerr := alloc.Allocate(ctx, tid, fy, invoicenumber.KindInvoice)
					if aerr != nil {
						return aerr
					}
					results <- n.Seq()
					return nil
				})
				if err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent allocate: %v", err)
	}
	seen := make(map[int64]bool, totalAlloc)
	var maxSeq int64
	for seq := range results {
		require.False(t, seen[seq], "duplicate sequence %d allocated", seq)
		seen[seq] = true
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	require.Len(t, seen, totalAlloc)
	require.Equal(t, int64(totalAlloc), maxSeq, "max allocated seq must equal total count — no gaps under contention")
}
