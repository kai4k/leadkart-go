package command_test

import (
	"context"
	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// ----- fakes for the reservation-specific ports ------------------------------

type fakeReservationLedger struct {
	count       int64
	outstanding []stockmovement.OutstandingReservation
	countErr    error
	listErr     error
}

func (f *fakeReservationLedger) CountReservationsForOrder(context.Context, tenant.ID, string) (int64, error) {
	return f.count, f.countErr
}

func (f *fakeReservationLedger) ListOutstandingReservationsForOrder(context.Context, tenant.ID, string) ([]stockmovement.OutstandingReservation, error) {
	return f.outstanding, f.listErr
}

type fakeEnqueuer struct{ events []integrationevents.Event }

func (f *fakeEnqueuer) EnqueueInTx(_ context.Context, events ...integrationevents.Event) error {
	f.events = append(f.events, events...)
	return nil
}

var (
	_ command.ReservationLedger = (*fakeReservationLedger)(nil)
	_ command.OutboxEnqueuer    = (*fakeEnqueuer)(nil)
)

func fixedClock() time.Time { return fixedNow }

// ----- ReserveOrderStock -----------------------------------------------------

func TestReserveOrderStock_ReservesAcrossFefoBatches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{count: 0}
	enq := &fakeEnqueuer{}
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-RES-1")
	b1 := seedBatchWithStock(t, batches, p, actor, "B1", 30)
	b2 := seedBatchLaterExpiryWithStock(t, batches, p, actor, "B2", 50)

	h := command.NewReserveOrderStockHandler(uow, batches, movements, ledger, enq, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(ctx, command.ReserveOrderStockCommand{
		TenantID:          p.TenantID(),
		OrderID:           uuid.NewString(),
		ActorMembershipID: actor,
		Lines:             []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.True(t, res.Reserved)
	require.Empty(t, res.Shortfalls)
	// FEFO: b1 (earlier expiry) takes 30, b2 takes the remaining 10.
	require.Equal(t, int64(30), b1.QuantityReserved())
	require.Equal(t, int64(10), b2.QuantityReserved())
	require.Equal(t, 2, countMovements(movements, batch.MovementReservation))
	require.Empty(t, enq.events)
}

func TestReserveOrderStock_AlreadyReservedIsNoOp(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{count: 2} // prior delivery already reserved
	enq := &fakeEnqueuer{}
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-RES-2")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)

	h := command.NewReserveOrderStockHandler(uow, batches, movements, ledger, enq, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
	})
	require.NoError(t, err)
	require.True(t, res.AlreadyReserved)
	require.False(t, res.Reserved)
	require.Zero(t, b.QuantityReserved())
	require.Equal(t, 0, movements.AddCalls)
}

func TestReserveOrderStock_ShortfallEmitsEventAndReservesNothing(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{count: 0}
	enq := &fakeEnqueuer{}
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-RES-3")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 10) // only 10 available

	orderID := uuid.NewString()
	h := command.NewReserveOrderStockHandler(uow, batches, movements, ledger, enq, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: p.TenantID(), OrderID: orderID, ActorMembershipID: actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.False(t, res.Reserved)
	require.Len(t, res.Shortfalls, 1)
	require.Equal(t, int64(40), res.Shortfalls[0].RequestedQuantity)
	require.Equal(t, int64(10), res.Shortfalls[0].AvailableQuantity)
	require.Zero(t, b.QuantityReserved(), "nothing reserved on shortfall")
	require.Equal(t, 0, movements.AddCalls)
	// shortfall event emitted for ops/notifications.
	require.Len(t, enq.events, 1)
	failed, ok := enq.events[0].(integrationevents.StockReservationFailedV1)
	require.True(t, ok)
	require.Equal(t, orderID, failed.OrderID.String())
	require.Len(t, failed.Shortfalls, 1)
}

// ----- ReleaseOrderStock -----------------------------------------------------

func TestReleaseOrderStock_ReleasesOutstandingHold(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-REL-1")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 30, fixedNow, fixedNow)) // hold 30
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{
		{BatchID: b.ID(), ProductID: p.ID(), Quantity: 30},
	}}

	h := command.NewReleaseOrderStockHandler(uow, batches, movements, ledger, fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReleaseOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor, Reason: "order cancelled",
	})
	require.NoError(t, err)
	require.True(t, res.Released)
	require.Equal(t, 1, res.BatchCount)
	require.Zero(t, b.QuantityReserved())
	require.Equal(t, int64(100), b.QuantityOnHand(), "release must not touch on-hand")
	require.Equal(t, 1, countMovements(movements, batch.MovementRelease))
}

func TestReleaseOrderStock_NothingOutstandingIsNoOp(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{outstanding: nil}
	actor := newMembershipID(t)

	h := command.NewReleaseOrderStockHandler(uow, batches, movements, ledger, fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReleaseOrderStockCommand{
		TenantID: newTenantID(t), OrderID: uuid.NewString(), ActorMembershipID: actor, Reason: "x",
	})
	require.NoError(t, err)
	require.False(t, res.Released)
	require.Equal(t, 0, movements.AddCalls)
}

// ----- ConsumeOrderStock -----------------------------------------------------

func TestConsumeOrderStock_ShipsAndReleasesHold(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-CON-1")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 40, fixedNow, fixedNow)) // hold 40
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{
		{BatchID: b.ID(), ProductID: p.ID(), Quantity: 40},
	}}

	h := command.NewConsumeOrderStockHandler(uow, batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ConsumeOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
	})
	require.NoError(t, err)
	require.True(t, res.Consumed)
	require.Equal(t, 1, res.BatchCount)
	require.Equal(t, int64(60), b.QuantityOnHand(), "shipped 40 of 100")
	require.Zero(t, b.QuantityReserved(), "hold released on consume")
	require.Equal(t, 1, countMovements(movements, batch.MovementRelease))
	require.Equal(t, 1, countMovements(movements, batch.MovementOutbound))
}

func TestConsumeOrderStock_NothingOutstandingIsNoOp(t *testing.T) {
	t.Parallel()
	uow := &fakeUoW{}
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{outstanding: nil}
	actor := newMembershipID(t)

	h := command.NewConsumeOrderStockHandler(uow, batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ConsumeOrderStockCommand{
		TenantID: newTenantID(t), OrderID: uuid.NewString(), ActorMembershipID: actor,
	})
	require.NoError(t, err)
	require.False(t, res.Consumed)
	require.Equal(t, 0, movements.AddCalls)
}

// Review B1: printed 2026-11, stop 0, and it is 05:00 IST on 1 December. The business
// day is the market's (no company can move it), so November stock is past its printed
// expiry: a shortfall, nothing reserved.
func TestReserveOrderStock_AtFiveAMIST_OnTheFirstOfTheNextMonth_PrintedExpiryIsPast(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-B1")
	b := seedBatchExpiryStock(t, batches, p, actor, "NOV", time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC), 10)

	pol := commercepolicy.Default()
	pol.Expiry.StopMonthsBefore = 0
	now := time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC) // 05:00 IST on 1 Dec
	h := command.NewReserveOrderStockHandler(&fakeUoW{}, batches, movements, &fakeReservationLedger{}, &fakeEnqueuer{}, sellabilitytest.ResolverWith(pol, now), func() time.Time { return now }, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
	})
	require.NoError(t, err)
	require.False(t, res.Reserved)
	require.Len(t, res.Shortfalls, 1)
	require.Zero(t, b.QuantityReserved())
}

// Review B2: units reserved while the batch was sellable cannot ship once its printed
// month is over: the consume refuses (ErrExpired), the whole transaction changes nothing.
func TestConsumeOrderStock_RefusesToShipUnitsOfABatchThatHasSinceExpired(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-B2")
	b := seedBatchExpiryStock(t, batches, p, actor, "JAN27", time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), 10)
	orderID := uuid.NewString()
	reserve := command.NewReserveOrderStockHandler(&fakeUoW{}, batches, movements, &fakeReservationLedger{}, &fakeEnqueuer{}, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := reserve.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: p.TenantID(), OrderID: orderID, ActorMembershipID: actor, Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
	})
	require.NoError(t, err)
	require.True(t, res.Reserved)

	later := time.Date(2027, 2, 10, 12, 0, 0, 0, time.UTC)
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{{BatchID: b.ID(), ProductID: p.ID(), Quantity: 10}}}
	consume := command.NewConsumeOrderStockHandler(&fakeUoW{}, batches, movements, ledger, sellabilitytest.Resolver(later), func() time.Time { return later }, testNewMovementID)
	_, err = consume.Handle(t.Context(), command.ConsumeOrderStockCommand{TenantID: p.TenantID(), OrderID: orderID, ActorMembershipID: actor})
	require.ErrorIs(t, err, batch.ErrExpired)
	require.Equal(t, int64(10), b.QuantityOnHand(), "nothing shipped")
}
