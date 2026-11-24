//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; each round is a few short
// tenant-scoped txs on two batches.

package adapters_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
)

// Review G2e (two workers): two releases of ONE order that run at the same time drain its
// hold once, and never touch the hold another order keeps on the same batch. The old release
// read the hold before its transaction, so both acted on the same snapshot and the second
// wiped the other order's units.
func TestReleaseOrderStock_ConcurrentReleasesDrainOnceAndLeaveTheOtherOrdersHold(t *testing.T) {
	rig := newReservationRig(t)
	for round := 0; round < 12; round++ {
		p, b := rig.seedStockedBatch(t, 100)
		orderA, orderB := uuid.NewString(), uuid.NewString()
		for _, o := range []string{orderA, orderB} {
			res, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
				TenantID: rig.tid, OrderID: o, ActorMembershipID: rig.actor,
				Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
			})
			require.NoError(t, err)
			require.True(t, res.Reserved)
		}

		const workers = 4
		var wg sync.WaitGroup
		start := make(chan struct{})
		released := make(chan bool, workers)
		errs := make(chan error, workers)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				res, err := rig.release.Handle(rig.ctx, command.ReleaseOrderStockCommand{
					TenantID: rig.tid, OrderID: orderA, ActorMembershipID: rig.actor, Reason: "order cancelled",
				})
				errs <- err
				released <- res.Released
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(released)
		for err := range errs {
			require.NoError(t, err)
		}
		drained := 0
		for r := range released {
			if r {
				drained++
			}
		}
		require.Equal(t, 1, drained, "round %d: the hold is drained by exactly one worker", round)

		got, err := rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
		require.NoError(t, err)
		require.Equal(t, int64(10), got.QuantityReserved(), "round %d: the other order's hold is intact", round)
		require.Equal(t, int64(100), got.QuantityOnHand())
	}
}
