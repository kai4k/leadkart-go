package command_test

import (
	"context"
	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
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

// ----- ShipOrderStock -------------------------------------------------------

func TestShipOrderStock_ShipsAndReleasesHold(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-CON-1")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 40, fixedNow, fixedNow)) // hold 40
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{
		{BatchID: b.ID(), ProductID: p.ID(), Quantity: 40},
	}}

	h := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ShipOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Expected: []command.ProductQuantity{{ProductID: p.ID().String(), Quantity: 40}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, res.BatchCount)
	require.Equal(t, int64(60), b.QuantityOnHand(), "shipped 40 of 100")
	require.Zero(t, b.QuantityReserved(), "hold released on shipment")
	require.Equal(t, 1, countMovements(movements, batch.MovementRelease))
	require.Equal(t, 1, countMovements(movements, batch.MovementOutbound))
}

// Review G2g Q8/M8: goods leave only for what was ordered. The hold read under the locks must
// EQUAL the confirmed quantity per product: less and more are refused and nothing moves.
func TestShipOrderStock_TheHoldMustEqualTheConfirmedQuantityPerProduct(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		held      int64
		confirmed int64
		wantErr   bool
	}{
		{"exact", 40, 40, false},
		{"short: held less than confirmed", 30, 40, true},
		{"over: held more than confirmed", 50, 40, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			batches := newFakeBatchRepo()
			movements := newFakeMovementRepo()
			actor := newMembershipID(t)
			p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-EQ-"+c.name[:4])
			b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
			require.NoError(t, b.ApplyMovement(batch.MovementReservation, c.held, fixedNow, fixedNow))
			ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{{BatchID: b.ID(), ProductID: p.ID(), Quantity: c.held}}}

			h := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
			_, err := h.Handle(t.Context(), command.ShipOrderStockCommand{
				TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
				Expected: []command.ProductQuantity{{ProductID: p.ID().String(), Quantity: c.confirmed}},
			})
			if c.wantErr {
				require.ErrorIs(t, err, command.ErrOrderStockMismatch)
				require.Equal(t, int64(100), b.QuantityOnHand(), "nothing shipped")
				require.Equal(t, c.held, b.QuantityReserved(), "the hold is untouched")
				require.Equal(t, 0, movements.AddCalls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(60), b.QuantityOnHand())
		})
	}
}

// A hold on a product the order did not confirm is a mismatch too, as is a missing expectation.
func TestShipOrderStock_AHoldOnAnotherProductOrNoExpectationIsRefused(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-EQ-OTHER")
	b := seedBatchWithStock(t, batches, p, actor, "B1", 100)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 10, fixedNow, fixedNow))
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{{BatchID: b.ID(), ProductID: p.ID(), Quantity: 10}}}
	h := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)

	_, err := h.Handle(t.Context(), command.ShipOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Expected: []command.ProductQuantity{{ProductID: "another-product", Quantity: 10}},
	})
	require.ErrorIs(t, err, command.ErrOrderStockMismatch)

	_, err = h.Handle(t.Context(), command.ShipOrderStockCommand{TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor})
	require.ErrorIs(t, err, batch.ErrInvalid)
	require.Equal(t, 0, movements.AddCalls)
}

func TestShipOrderStock_NothingHeldIsRefused(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	ledger := &fakeReservationLedger{outstanding: nil}
	actor := newMembershipID(t)

	h := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(fixedNow), fixedClock, testNewMovementID)
	res, err := h.Handle(t.Context(), command.ShipOrderStockCommand{
		TenantID: newTenantID(t), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Expected: []command.ProductQuantity{{ProductID: "some-product", Quantity: 1}},
	})
	require.ErrorIs(t, err, command.ErrOrderStockNotHeld, "goods never leave without a deduction")
	require.Zero(t, res.BatchCount)
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
// month is over: the shipment refuses (ErrExpired), the whole transaction changes nothing.
func TestShipOrderStock_RefusesToShipUnitsOfABatchThatHasSinceExpired(t *testing.T) {
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
	consume := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(later), func() time.Time { return later }, testNewMovementID)
	_, err = consume.Handle(t.Context(), command.ShipOrderStockCommand{TenantID: p.TenantID(), OrderID: orderID, ActorMembershipID: actor, Expected: []command.ProductQuantity{{ProductID: p.ID().String(), Quantity: 10}}})
	require.ErrorIs(t, err, batch.ErrExpired)
	require.Equal(t, int64(10), b.QuantityOnHand(), "nothing shipped")
}

// Review L2: the shipment is judged on the company BUSINESS day, not the UTC date. At 20:00 UTC on
// 31 January it is already 1 February at the market offset (+05:30): a batch printed 2027-01 is
// past its printed expiry, so its reserved units cannot ship. Judged on the UTC date the shipment
// would go out for another five and a half hours.
func TestShipOrderStock_IsJudgedOnTheBusinessDayNotTheUTCDate(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-L2")
	b := seedBatchExpiryStock(t, batches, p, actor, "JAN27", time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC), 10)
	require.NoError(t, b.ApplyMovement(batch.MovementReservation, 10, fixedNow, fixedNow))

	now := time.Date(2027, 1, 31, 20, 0, 0, 0, time.UTC) // 01:30 IST on 1 February
	ledger := &fakeReservationLedger{outstanding: []stockmovement.OutstandingReservation{{BatchID: b.ID(), ProductID: p.ID(), Quantity: 10}}}
	consume := command.NewShipOrderStockHandler(batches, movements, ledger, sellabilitytest.Resolver(now), func() time.Time { return now }, testNewMovementID)
	_, err := consume.Handle(t.Context(), command.ShipOrderStockCommand{TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor, Expected: []command.ProductQuantity{{ProductID: p.ID().String(), Quantity: 10}}})
	require.ErrorIs(t, err, batch.ErrExpired)
	require.Equal(t, int64(10), b.QuantityOnHand(), "nothing shipped")
}

// Review S6: the plan was made on one business day and the transaction runs after midnight:
// a batch whose cut-off passed in between is refused inside the transaction, nothing reserved.
func TestReserveOrderStock_RechecksSellabilityInsideTheTransaction(t *testing.T) {
	t.Parallel()
	batches := newFakeBatchRepo()
	movements := newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, newFakeProductRepo(), newTenantID(t), actor, "SKU-S6")
	b := seedBatchExpiryStock(t, batches, p, actor, "MAY", time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), 10)

	pol := commercepolicy.Default()
	pol.Expiry.StopMonthsBefore = 0
	clocks := []time.Time{time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)} // plan, then the transaction
	var calls int
	resolver := sellability.NewResolver(sellabilitytest.Policy{Value: commercepolicy.Versioned{Version: 1, Policy: pol}}, func() time.Time {
		c := clocks[min(calls, len(clocks)-1)]
		calls++
		return c
	})
	h := command.NewReserveOrderStockHandler(&fakeUoW{}, batches, movements, &fakeReservationLedger{}, &fakeEnqueuer{}, resolver, fixedClock, testNewMovementID)
	_, err := h.Handle(t.Context(), command.ReserveOrderStockCommand{
		TenantID: p.TenantID(), OrderID: uuid.NewString(), ActorMembershipID: actor,
		Lines: []command.ReserveLine{{ProductID: p.ID(), Quantity: 10}},
	})
	require.ErrorIs(t, err, batch.ErrExpired)
	require.Zero(t, b.QuantityReserved())
}
