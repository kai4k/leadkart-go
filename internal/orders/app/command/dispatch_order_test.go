package command_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order/ordertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// seedOrderAt persists an order walked to the given state (packed, invoiced or cancelled).
func seedOrderAt(t *testing.T, repo *ordertest.FakeRepository, tid tenant.ID, actor membership.ID, target order.State) order.ID {
	t.Helper()
	o, err := order.New(order.NewInput{
		ID: order.ID(uuid.NewString()), TenantID: tid,
		ApprovedQuotationID: quotation.ID(uuid.NewString()), CustomerLeadID: quotation.CustomerLeadID(uuid.NewString()),
		ConfirmedItems: []quotation.LineItem{{
			ProductID: uuid.NewString(), SKU: "SKU-1", Quantity: 10,
			UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200,
		}},
		CreatedByMembershipID: actor, Now: now(),
	})
	require.NoError(t, err)
	require.NoError(t, o.RecordTokenPayment(actor, now()))
	require.NoError(t, o.Confirm(actor, now()))
	require.NoError(t, o.MarkPacked(actor, now()))
	switch target {
	case order.StateInvoiced, order.StateDispatched, order.StateCancelled:
		require.NoError(t, o.AttachInvoice("inv-1", actor, now()))
	}
	if target == order.StateDispatched {
		require.NoError(t, o.MarkDispatched(actor, now()))
	}
	if target == order.StateCancelled {
		require.NoError(t, o.Cancel("changed mind", actor, now()))
	}
	o.PullEvents()
	require.NoError(t, repo.Add(t.Context(), o))
	return o.ID()
}

func TestDispatchOrder_AdvancesAnInvoicedOrder(t *testing.T) {
	t.Parallel()
	tid, actor := tenant.ID(uuid.NewString()), membership.ID(uuid.NewString())
	repo := ordertest.NewFakeRepository()
	id := seedOrderAt(t, repo, tid, actor, order.StateInvoiced)

	require.NoError(t, command.NewDispatchOrderHandler(repo, now).Handle(t.Context(), command.DispatchOrderCommand{
		TenantID: tid, OrderID: id, ConsignmentNoteID: uuid.NewString(), TransitionedByMembership: actor,
	}))

	got, err := repo.GetByID(t.Context(), tid, id)
	require.NoError(t, err)
	require.Equal(t, order.StateDispatched, got.State())
	require.NotNil(t, got.DispatchedAt())
}

// Review G2g M4: the note is linked in the dispatch transaction itself, so a lost or late
// consignment-created event cannot leave a dispatched order with no note. The async attach of
// the same note stays a no-op.
func TestDispatchOrder_LinksTheNoteInTheSameStep(t *testing.T) {
	t.Parallel()
	tid, actor := tenant.ID(uuid.NewString()), membership.ID(uuid.NewString())
	repo := ordertest.NewFakeRepository()
	id := seedOrderAt(t, repo, tid, actor, order.StateInvoiced)
	noteID := uuid.NewString()

	require.NoError(t, command.NewDispatchOrderHandler(repo, now).Handle(t.Context(), command.DispatchOrderCommand{
		TenantID: tid, OrderID: id, ConsignmentNoteID: noteID, TransitionedByMembership: actor,
	}))

	got, err := repo.GetByID(t.Context(), tid, id)
	require.NoError(t, err)
	require.Equal(t, noteID, got.ConsignmentNoteID())

	// And it reports what the order confirmed, per product (the stock step's expectation).
	other := seedOrderAt(t, repo, tid, actor, order.StateInvoiced)
	confirmed, err := command.NewDispatchOrderHandler(repo, now).Dispatch(t.Context(), command.DispatchOrderCommand{
		TenantID: tid, OrderID: other, ConsignmentNoteID: uuid.NewString(), TransitionedByMembership: actor,
	})
	require.NoError(t, err)
	require.Len(t, confirmed, 1)
	require.Equal(t, int64(10), confirmed[0].Quantity)
	require.NoError(t, command.NewAttachConsignmentHandler(repo).Handle(t.Context(), command.AttachConsignmentCommand{
		TenantID: tid, OrderID: id, ConsignmentNoteID: noteID,
	}), "the async attach of the same note is a no-op")
}

func TestDispatchOrder_RefusalsLeaveTheOrderAlone(t *testing.T) {
	t.Parallel()
	tid, actor := tenant.ID(uuid.NewString()), membership.ID(uuid.NewString())
	for _, c := range []struct {
		name   string
		state  order.State
		wantIs error
	}{
		{"packed, no invoice", order.StatePacked, command.ErrDispatchNotInvoiced},
		{"cancelled, invoice kept", order.StateCancelled, command.ErrDispatchWrongState},
		{"already dispatched", order.StateDispatched, command.ErrDispatchWrongState},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := ordertest.NewFakeRepository()
			id := seedOrderAt(t, repo, tid, actor, c.state)
			err := command.NewDispatchOrderHandler(repo, now).Handle(t.Context(), command.DispatchOrderCommand{
				TenantID: tid, OrderID: id, ConsignmentNoteID: uuid.NewString(), TransitionedByMembership: actor,
			})
			require.ErrorIs(t, err, c.wantIs)
			got, gerr := repo.GetByID(t.Context(), tid, id)
			require.NoError(t, gerr)
			require.Equal(t, c.state, got.State())
		})
	}
}

func TestDispatchOrder_NoSuchOrderIsNotInvoiced(t *testing.T) {
	t.Parallel()
	tid, actor := tenant.ID(uuid.NewString()), membership.ID(uuid.NewString())
	repo := ordertest.NewFakeRepository()
	err := command.NewDispatchOrderHandler(repo, now).Handle(t.Context(), command.DispatchOrderCommand{
		TenantID: tid, OrderID: order.ID(uuid.NewString()), ConsignmentNoteID: uuid.NewString(), TransitionedByMembership: actor,
	})
	require.ErrorIs(t, err, command.ErrDispatchNotInvoiced)
}
