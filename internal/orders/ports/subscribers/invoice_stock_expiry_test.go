package subscribers_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// Review G2c B1: an event that arrives after a held batch's printed month ended must not
// produce a tax invoice. The refusal is permanent (a retry cannot change it), so it is
// marked non-retryable and goes to the dead letters for a person; the order stays packed.

func TestAutoInvoice_ExpiredHeldStockIsRefusedWithoutRetry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.stock.expired = true

	err := f.autoInvoice.Handle(t.Context(), &integrationevents.OrderPackedV1{
		OrderID: f.orderID.String(), TenantID: f.tenantID.String(), PackedByMembershipID: f.actor.String(),
	})

	require.ErrorIs(t, err, order.ErrStockExpired)
	require.True(t, messaging.IsNonRetryable(err), "a retry cannot make expired stock fresh")
	require.Equal(t, order.StatePacked, f.orderState(t))
	_, ierr := f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.Error(t, ierr, "no invoice may exist")
}

// The consignment events never invoice or dispatch (review G2g), so expired held stock
// cannot slip an invoice or a shipment through them: the created event only links the note,
// and the delivered event of an order that never left is a conflict.
func TestConsignmentSaga_NeverInvoicesOrDispatchesWhateverTheStock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.stock.expired = true

	require.NoError(t, f.created.Handle(t.Context(), f.createdEvent(uuid.New())))
	require.Equal(t, order.StatePacked, f.orderState(t), "no invoice, so no dispatch")
	_, ierr := f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.Error(t, ierr)

	err := f.delivered.Handle(t.Context(), f.deliveredEvent(uuid.New()))
	require.Error(t, err)
	require.Equal(t, order.StatePacked, f.orderState(t))
	_, ierr = f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.Error(t, ierr)
}
