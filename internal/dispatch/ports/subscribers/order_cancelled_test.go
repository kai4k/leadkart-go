package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

func buildCancelHandler(t *testing.T) (*subscribers.OrderCancelledSubscriber, *consignmentnotetest.FakeRepository) {
	t.Helper()
	repo := consignmentnotetest.NewFakeRepository()
	return subscribers.NewOrderCancelledSubscriber(
		query.NewGetConsignmentNoteByOrderHandler(repo),
		command.NewMarkFailedHandler(repo, fixedNow),
		silentLog(),
	), repo
}

func seedPendingNote(t *testing.T, repo *consignmentnotetest.FakeRepository, tenantID tenant.ID, orderID consignmentnote.OrderID) *consignmentnote.ConsignmentNote {
	t.Helper()
	cn, err := consignmentnote.New(consignmentnote.NewInput{
		ID:                    consignmentnote.ID(ids.NewV7().String()),
		TenantID:              tenantID,
		OrderID:               orderID,
		CarrierName:           "BlueDart",
		BoxCount:              1,
		WeightGrams:           500,
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
		Now:                   fixedNow(),
	})
	require.NoError(t, err)
	cn.PullEvents()
	require.NoError(t, repo.Add(t.Context(), cn))
	return cn
}

func cancelledEvent(tenantID, orderID, priorState string) *ordersevents.OrderCancelledV1 {
	return &ordersevents.OrderCancelledV1{
		OrderID:               orderID,
		TenantID:              tenantID,
		PriorState:            priorState,
		Reason:                "customer withdrew",
		CancelledAtUTC:        fixedNow(),
		CancelledByMembership: ids.NewV7().String(),
	}
}

// TestOrderCancelled_FailsConsignment: order cancelled at `packed` with the
// consignment slot already minted → the note flips to failed with the
// cancellation reason; replay is a self-idempotent ack.
func TestOrderCancelled_FailsConsignment(t *testing.T) {
	t.Parallel()
	h, repo := buildCancelHandler(t)
	tenantID := tenant.ID(ids.NewV7().String())
	orderID := consignmentnote.OrderID(ids.NewV7().String())
	cn := seedPendingNote(t, repo, tenantID, orderID)
	evt := cancelledEvent(tenantID.String(), orderID.String(), "packed")

	require.NoError(t, h.Handle(t.Context(), evt))
	got := repo.Store[cn.ID()]
	require.Equal(t, consignmentnote.StatusFailed, got.Status())
	require.Equal(t, "order cancelled: customer withdrew", got.FailureReason())

	require.NoError(t, h.Handle(t.Context(), evt), "replay must ack")
}

// TestOrderCancelled_PreConsignmentStatesAck: cancels before packing never
// have a consignment — the handler acks immediately instead of retrying for a
// slot that will never exist.
func TestOrderCancelled_PreConsignmentStatesAck(t *testing.T) {
	t.Parallel()
	h, _ := buildCancelHandler(t)
	for _, prior := range []string{"quotation_approved", "token_paid", "confirmed"} {
		evt := cancelledEvent(ids.NewV7().String(), ids.NewV7().String(), prior)
		require.NoError(t, h.Handle(t.Context(), evt), "prior=%s", prior)
	}
}

// TestOrderCancelled_SlotNotYetVisibleRetries: PriorState says a slot exists
// (packed) but the order_packed ingestor hasn't minted it yet → the handler
// errors so the broker redelivers until the slot is visible.
func TestOrderCancelled_SlotNotYetVisibleRetries(t *testing.T) {
	t.Parallel()
	h, _ := buildCancelHandler(t)
	evt := cancelledEvent(ids.NewV7().String(), ids.NewV7().String(), "packed")
	require.Error(t, h.Handle(t.Context(), evt))
}
