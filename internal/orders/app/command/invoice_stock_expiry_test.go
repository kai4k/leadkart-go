package command_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice/invoicetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber/invoicenumbertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// Review G2c B1: the expiry guard at the pack is not enough, because the invoice is issued
// by an event that can arrive after the printed month ends. Whatever route reaches the
// invoice (auto-invoice, the manual route, the consignment saga's ensure-invoiced), no tax
// invoice is issued on stock that is past its printed expiry; the order stays packed.
func TestInvoiceOrder_RefusesAnOrderHoldingExpiredStockHoweverLateItIsAsked(t *testing.T) {
	t.Parallel()
	orders, tid, orderID, actor := confirmedOrder(t)
	require.NoError(t, command.NewPackOrderHandler(fakeUoW{}, orders, &fakeEnqueuer{}, stockFresh(), now).Handle(t.Context(), command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500, PackedByMembership: actor,
	}))
	invoices := invoicetest.NewFakeRepository()
	guard := &fakeStockGuard{expired: true} // the printed month ended after the pack
	h := command.NewInvoiceOrderHandler(fakeUoW{}, orders, invoices, invoicenumbertest.NewFakeAllocator(), guard, now, invID)

	_, err := h.Handle(t.Context(), command.InvoiceOrderCommand{TenantID: tid, OrderID: orderID, IssuedByMembership: actor})

	require.ErrorIs(t, err, order.ErrStockExpired)
	require.Equal(t, 1, guard.asked)
	got, gerr := orders.GetByID(t.Context(), tid, orderID)
	require.NoError(t, gerr)
	require.Equal(t, order.StatePacked, got.State(), "the order stays packed, so it can still be cancelled")
	_, ierr := invoices.GetByOrderID(t.Context(), tid, orderID)
	require.Error(t, ierr, "no invoice may exist")
}

// A guard that cannot answer is not "fresh": no invoice is issued blind.
func TestInvoiceOrder_FailsWhenTheStockGuardCannotAnswer(t *testing.T) {
	t.Parallel()
	orders, tid, orderID, actor := confirmedOrder(t)
	require.NoError(t, command.NewPackOrderHandler(fakeUoW{}, orders, &fakeEnqueuer{}, stockFresh(), now).Handle(t.Context(), command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500, PackedByMembership: actor,
	}))
	boom := errors.New("inventory down")
	h := command.NewInvoiceOrderHandler(fakeUoW{}, orders, invoicetest.NewFakeRepository(), invoicenumbertest.NewFakeAllocator(), &fakeStockGuard{err: boom}, now, invID)

	_, err := h.Handle(t.Context(), command.InvoiceOrderCommand{TenantID: tid, OrderID: orderID, IssuedByMembership: actor})

	require.ErrorIs(t, err, boom)
	got, gerr := orders.GetByID(t.Context(), tid, orderID)
	require.NoError(t, gerr)
	require.Equal(t, order.StatePacked, got.State())
}
