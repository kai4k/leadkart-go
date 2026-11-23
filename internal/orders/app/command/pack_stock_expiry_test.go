package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// fakeStockGuard answers the Inventory question "does this order hold stock past
// its printed expiry?" with a fixed answer.
type fakeStockGuard struct {
	expired bool
	err     error
	asked   int
}

func (g *fakeStockGuard) HoldsExpiredStock(context.Context, tenant.ID, order.ID) (bool, error) {
	g.asked++
	return g.expired, g.err
}

// stockFresh is the guard that finds nothing expired, for tests that are not about expiry.
func stockFresh() *fakeStockGuard { return &fakeStockGuard{} }

// An order that holds stock past its printed expiry is never packed: expired stock is
// never sold, and the pack is what starts the invoice and the consignment note.
// The order stays confirmed (so it can be cancelled) and nothing is published.
func TestPackOrder_RefusesAnOrderHoldingExpiredStock(t *testing.T) {
	t.Parallel()
	orders, tid, orderID, actor := confirmedOrder(t)
	enq := &fakeEnqueuer{}
	guard := &fakeStockGuard{expired: true}

	err := command.NewPackOrderHandler(fakeUoW{}, orders, enq, guard, now).Handle(t.Context(), command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500, PackedByMembership: actor,
	})

	require.ErrorIs(t, err, order.ErrStockExpired)
	require.Equal(t, 1, guard.asked)
	got, gerr := orders.GetByID(t.Context(), tid, orderID)
	require.NoError(t, gerr)
	require.Equal(t, order.StateConfirmed, got.State(), "a refused pack must not move the order")
	require.False(t, hasEvent(enq.events, integrationevents.TopicOrderPackedV1), "nothing may be published")
}

// A guard that cannot answer is not "fresh": the pack fails rather than ship blind.
func TestPackOrder_FailsWhenTheStockGuardCannotAnswer(t *testing.T) {
	t.Parallel()
	orders, tid, orderID, actor := confirmedOrder(t)
	enq := &fakeEnqueuer{}
	boom := errors.New("inventory down")

	err := command.NewPackOrderHandler(fakeUoW{}, orders, enq, &fakeStockGuard{err: boom}, now).Handle(t.Context(), command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500, PackedByMembership: actor,
	})

	require.ErrorIs(t, err, boom)
	got, gerr := orders.GetByID(t.Context(), tid, orderID)
	require.NoError(t, gerr)
	require.Equal(t, order.StateConfirmed, got.State())
	require.False(t, hasEvent(enq.events, integrationevents.TopicOrderPackedV1))
}
