package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeInvoiced answers Orders' question "does this order have a tax invoice?".
type fakeInvoiced struct {
	has bool
	err error
}

func (f fakeInvoiced) OrderHasInvoice(context.Context, tenant.ID, string) (bool, error) {
	return f.has, f.err
}

// Review G2d B1: goods do not leave without a tax invoice. A note whose order has no invoice
// (the invoice was refused on expired stock, or the order was never packed) stays pending.
func TestMarkDispatchedHandler_RefusesAnOrderWithoutAnInvoice(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)

	err := command.NewMarkDispatchedHandler(repo, fakeInvoiced{has: false}, fixedNow).Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "BDX-1", TransitionedByMembership: actor,
	})

	require.ErrorIs(t, err, command.ErrOrderNotInvoiced)
	got, gerr := repo.GetByID(t.Context(), tID, cnID)
	require.NoError(t, gerr)
	require.Equal(t, consignmentnote.StatusPending, got.Status())
}

// A read that cannot answer is not "invoiced": the dispatch fails closed.
func TestMarkDispatchedHandler_FailsClosedWhenTheInvoiceCannotBeRead(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	boom := errors.New("orders down")

	err := command.NewMarkDispatchedHandler(repo, fakeInvoiced{err: boom}, fixedNow).Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "BDX-1", TransitionedByMembership: actor,
	})

	require.ErrorIs(t, err, boom)
	require.NotErrorIs(t, err, command.ErrOrderNotInvoiced)
	got, gerr := repo.GetByID(t.Context(), tID, cnID)
	require.NoError(t, gerr)
	require.Equal(t, consignmentnote.StatusPending, got.Status())
}
