package command_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/parcel"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order/ordertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment/paymenttest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation/quotationtest"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// confirmedOrder drives quote → approve → token → confirm and returns the
// repo + ids, so a test can then try to pack.
func confirmedOrder(t *testing.T) (*ordertest.FakeRepository, tenant.ID, order.ID, membership.ID) {
	t.Helper()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	ctx := t.Context()
	quotes := quotationtest.NewFakeRepository()
	orders := ordertest.NewFakeRepository()
	payments := paymenttest.NewFakeRepository()
	enq := &fakeEnqueuer{}
	uow := fakeUoW{}

	quoteID, err := command.NewCreateQuotationHandler(quotes, now, qID).Handle(ctx, command.CreateQuotationCommand{
		TenantID: tid, CustomerLeadID: uuid.NewString(),
		Items: []command.CreateQuotationLineItem{sampleItem()}, CreatedByMembershipID: actor,
	})
	require.NoError(t, err)
	appr, err := command.NewApproveQuotationHandler(uow, quotes, orders, now, oID).Handle(ctx, command.ApproveQuotationCommand{
		TenantID: tid, QuotationID: quoteID, ApprovedByMembership: actor,
	})
	require.NoError(t, err)
	require.NoError(t, command.NewRecordTokenPaymentHandler(uow, orders, payments, now, payID).Handle(ctx, command.RecordTokenPaymentCommand{
		TenantID: tid, OrderID: appr.OrderID, Method: "upi", AmountPaise: 25000, RecordedByMembership: actor,
	}))
	require.NoError(t, command.NewConfirmOrderHandler(uow, orders, enq, now).Handle(ctx, command.ConfirmOrderCommand{
		TenantID: tid, OrderID: appr.OrderID, ConfirmedByMembership: actor,
	}))
	return orders, tid, appr.OrderID, actor
}

// TestPackOrder_RefusesWhatDispatchWouldRefuse is the jam scenario: a pack the
// consignment note's rules would refuse must be refused up front, with a
// stable code, leaving the order confirmed and publishing nothing. Before the
// fix the order moved to packed and the retried subscriber never succeeded.
func TestPackOrder_RefusesWhatDispatchWouldRefuse(t *testing.T) {
	t.Parallel()
	past := now().Add(-24 * time.Hour)
	cases := []struct {
		name string
		edit func(*command.PackOrderCommand)
		code string
	}{
		{"blank carrier", func(c *command.PackOrderCommand) { c.CarrierName = "  " }, parcel.CodeCarrierRequired},
		// I2: a NUL passes JSON and the json outbox column, then the consignment
		// note insert fails (SQLSTATE 22021) on every delivery of the event.
		{"NUL in carrier", func(c *command.PackOrderCommand) { c.CarrierName = "Blue\x00Dart" }, parcel.CodeCarrierInvalidCharacters},
		{"zero boxes", func(c *command.PackOrderCommand) { c.BoxCount = 0 }, parcel.CodeBoxCountInvalid},
		{"zero weight", func(c *command.PackOrderCommand) { c.WeightGrams = 0 }, parcel.CodeWeightInvalid},
		{"past delivery", func(c *command.PackOrderCommand) { c.ExpectedDeliveryAt = &past }, parcel.CodeDeliveryNotInFuture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			orders, tid, orderID, actor := confirmedOrder(t)
			enq := &fakeEnqueuer{}
			cmd := command.PackOrderCommand{
				TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
				PackedByMembership: actor,
			}
			tc.edit(&cmd)

			err := command.NewPackOrderHandler(fakeUoW{}, orders, enq, stockFresh(), now).Handle(t.Context(), cmd)

			var pe *parcel.Error
			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.code, pe.Code)
			got, gerr := orders.GetByID(t.Context(), tid, orderID)
			require.NoError(t, gerr)
			require.Equal(t, order.StateConfirmed, got.State(), "a refused pack must not move the order")
			require.False(t, hasEvent(enq.events, integrationevents.TopicOrderPackedV1), "nothing may be published")
		})
	}
}

// The pack rules and the event's PackedAtUTC come from ONE clock reading: a
// delivery date valid at the check can never already be past at the PackedAtUTC
// Dispatch judges the consignment note at.
func TestPackOrder_JudgesAndStampsWithOneClockReading(t *testing.T) {
	t.Parallel()
	orders, tid, orderID, actor := confirmedOrder(t)
	enq := &fakeEnqueuer{}
	tick := now()
	clock := func() time.Time { tick = tick.Add(time.Second); return tick } // every reading is later
	eta := tick.Add(1500 * time.Millisecond)                                // future at the 1st reading, past at the 2nd

	err := command.NewPackOrderHandler(fakeUoW{}, orders, enq, stockFresh(), clock).Handle(t.Context(), command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500,
		ExpectedDeliveryAt: &eta, PackedByMembership: actor,
	})
	require.NoError(t, err)
	var packed integrationevents.OrderPackedV1
	for _, e := range enq.events {
		if p, ok := e.(integrationevents.OrderPackedV1); ok {
			packed = p
		}
	}
	require.False(t, packed.PackedAtUTC.IsZero(), "the packed event must be published")
	require.NoError(t, parcel.Validate(parcel.Details{
		CarrierName: packed.CarrierName, BoxCount: packed.BoxCount, WeightGrams: packed.WeightGrams,
		ExpectedDeliveryAt: packed.ExpectedDeliveryAtUTC,
	}, packed.PackedAtUTC), "what Orders accepted must be valid at the PackedAtUTC Dispatch judges it at")
}
