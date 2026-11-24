//go:build integration

// arch-test:no-timeout-needed — shared pgtest container; each case is a handful
// of short tenant-scoped txs (reserve/release/consume on one batch).

package adapters_test

import (
	"context"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// reservationRig bundles the wired handlers + repos for a reservation test.
type reservationRig struct {
	ctx     context.Context
	tid     tenant.ID
	actor   membership.ID
	batches *adapters.BatchRepository
	reserve command.ReserveOrderStockHandler
	release command.ReleaseOrderStockHandler
	shipper command.ShipOrderStockHandler
	tx      *pg.Transactor
}

func newReservationRig(t *testing.T) reservationRig {
	t.Helper()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	tx := pg.NewTransactor(pool)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	enq := adapters.NewOutboxEnqueuer()
	clk := func() time.Time { return fixedNow }
	newMID := func() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }
	return reservationRig{
		ctx:     ctx,
		tid:     tid,
		actor:   membership.ID(ids.NewV7().String()),
		batches: batches,
		// movements satisfies both stockmovement.Repository (writer) and
		// command.ReservationLedger (the outstanding-by-batch reads).
		reserve: command.NewReserveOrderStockHandler(tx, batches, movements, movements, enq, sellabilitytest.Resolver(fixedNow), clk, newMID),
		release: command.NewReleaseOrderStockHandler(tx, batches, movements, movements, clk, newMID),
		shipper: command.NewShipOrderStockHandler(batches, movements, movements, sellabilitytest.Resolver(fixedNow), clk, newMID),
		tx:      tx,
	}
}

// ship runs the shipment the way Dispatch does: as one step of a transaction the caller owns.
func (r reservationRig) ship(orderID string) (command.ShipOrderStockResult, error) {
	var res command.ShipOrderStockResult
	err := r.tx.WithinTx(r.ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var herr error
		res, herr = r.shipper.Handle(ctx, command.ShipOrderStockCommand{TenantID: r.tid, OrderID: orderID, ActorMembershipID: r.actor})
		return herr
	})
	return res, err
}

// seedStockedBatch persists a product + a live batch carrying onHand units.
func (r reservationRig) seedStockedBatch(t *testing.T, onHand int64) (*product.Product, *batch.Batch) {
	t.Helper()
	tx := pg.NewTransactor(repoFixture(t))
	products := adapters.NewProductRepository(repoFixture(t), tx)
	batches := adapters.NewBatchRepository(repoFixture(t), tx)
	p, err := product.New(product.ID(ids.NewV7().String()), r.tid, r.actor,
		product.Spec{SKU: "RES-" + ids.NewV7().String()[28:], Name: "Res Drug", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	require.NoError(t, err)
	require.NoError(t, products.Add(r.ctx, p))

	b, err := batch.New(batch.ID(ids.NewV7().String()), p.ID(), r.tid, r.actor,
		batch.Spec{BatchNumber: "LOT-" + ids.NewV7().String()[28:],
			ManufactureDate:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			ExpiryDate:       time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
			ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1",
			MRPPaise: 25000, PurchasePricePaise: 18000}, fixedNow)
	require.NoError(t, err)
	require.NoError(t, b.ApplyMovement(batch.MovementInbound, onHand, fixedNow, fixedNow))
	require.NoError(t, batches.Add(r.ctx, b))
	return p, b
}

// TestReservationLifecycle_ReserveReleaseRoundTrip pins the cancel path: reserve
// holds available stock (oversell guard), a redelivery is idempotent, and
// release returns the hold without touching on-hand. A second release no-ops.
func TestReservationLifecycle_ReserveReleaseRoundTrip(t *testing.T) {
	rig := newReservationRig(t)
	p, b := rig.seedStockedBatch(t, 100)
	orderID := uuid.NewString()

	res, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.True(t, res.Reserved)

	got, err := rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Equal(t, int64(40), got.QuantityReserved())
	require.Equal(t, int64(60), got.AvailableQuantity())
	require.Equal(t, int64(100), got.QuantityOnHand())

	// Idempotent re-reserve: AlreadyReserved, hold unchanged.
	res2, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.True(t, res2.AlreadyReserved)
	got, err = rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Equal(t, int64(40), got.QuantityReserved())

	// Release on cancel.
	rres, err := rig.release.Handle(rig.ctx, command.ReleaseOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	require.True(t, rres.Released)
	require.Equal(t, 1, rres.BatchCount)

	got, err = rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Zero(t, got.QuantityReserved())
	require.Equal(t, int64(100), got.QuantityOnHand())

	// Idempotent release: nothing outstanding → no-op.
	rres2, err := rig.release.Handle(rig.ctx, command.ReleaseOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	require.False(t, rres2.Released)
}

// TestReservationLifecycle_ReserveShipShipsStock pins the dispatch path: the shipment
// realises the hold (reserved−q AND on-hand−q). A second shipment of the same order is
// refused, never a second deduction.
func TestReservationLifecycle_ReserveShipShipsStock(t *testing.T) {
	rig := newReservationRig(t)
	p, b := rig.seedStockedBatch(t, 100)
	orderID := uuid.NewString()

	_, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)

	cres, err := rig.ship(orderID)
	require.NoError(t, err)
	require.Equal(t, 1, cres.BatchCount)

	got, err := rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Equal(t, int64(60), got.QuantityOnHand(), "shipped 40 of 100")
	require.Zero(t, got.QuantityReserved())

	// A second shipment: nothing held → refused, on-hand stable.
	_, err = rig.ship(orderID)
	require.ErrorIs(t, err, command.ErrOrderStockNotHeld)
	got, err = rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Equal(t, int64(60), got.QuantityOnHand())
}

// TestReservation_OversellGuard pins the headline guarantee: once an order
// reserves stock, a second order cannot reserve the held units — the second
// reservation reports a shortfall and reserves nothing.
func TestReservation_OversellGuard(t *testing.T) {
	rig := newReservationRig(t)
	p, b := rig.seedStockedBatch(t, 50)

	first, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: uuid.NewString(), ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.True(t, first.Reserved)

	// Only 10 available now; a second order wanting 20 must fail.
	second, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: uuid.NewString(), ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 20}},
	})
	require.NoError(t, err)
	require.False(t, second.Reserved)
	require.Len(t, second.Shortfalls, 1)
	require.Equal(t, int64(20), second.Shortfalls[0].RequestedQuantity)
	require.Equal(t, int64(10), second.Shortfalls[0].AvailableQuantity)

	// The first order's hold is intact; nothing leaked to the second.
	got, err := rig.batches.GetByID(rig.ctx, rig.tid, b.ID())
	require.NoError(t, err)
	require.Equal(t, int64(40), got.QuantityReserved())
	require.Equal(t, int64(10), got.AvailableQuantity())
}

// TestReservationLifecycle_HeldStockExpiryIsAskedOnTheBusinessDay pins the pre-pack
// question Orders asks: an order that holds units on a batch whose printed month
// is over is flagged; once the hold is released or consumed it is not.
func TestReservationLifecycle_HeldStockExpiryIsAskedOnTheBusinessDay(t *testing.T) {
	rig := newReservationRig(t)
	movements := adapters.NewStockMovementRepository(repoFixture(t), pg.NewTransactor(repoFixture(t)))
	p, b := rig.seedStockedBatch(t, 20) // expires 31 Jan 2028
	orderID := uuid.NewString()
	_, err := rig.reserve.Handle(rig.ctx, command.ReserveOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
	})
	require.NoError(t, err)
	lastDay := b.ExpiryDate()
	firstOfNext := lastDay.AddDate(0, 0, 1)

	n, err := movements.CountExpiredBatchesHeldByOrder(rig.ctx, rig.tid, orderID, lastDay)
	require.NoError(t, err)
	require.Zero(t, n, "on the last day of the printed month the batch is not past it")

	n, err = movements.CountExpiredBatchesHeldByOrder(rig.ctx, rig.tid, orderID, firstOfNext)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "the day after the printed month the held batch is expired")

	n, err = movements.CountExpiredBatchesHeldByOrder(rig.ctx, rig.tid, uuid.NewString(), firstOfNext)
	require.NoError(t, err)
	require.Zero(t, n, "another order holds nothing")

	_, err = rig.release.Handle(rig.ctx, command.ReleaseOrderStockCommand{
		TenantID: rig.tid, OrderID: orderID, ActorMembershipID: rig.actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	n, err = movements.CountExpiredBatchesHeldByOrder(rig.ctx, rig.tid, orderID, firstOfNext)
	require.NoError(t, err)
	require.Zero(t, n, "a released hold is no longer held")
}
