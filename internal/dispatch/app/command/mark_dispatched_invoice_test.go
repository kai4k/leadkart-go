package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote/consignmentnotetest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeOrder answers Orders' question "may goods leave for this order?" (the zero value: yes).
type fakeOrder struct {
	status command.OrderDispatchStatus
	err    error
}

func (f fakeOrder) OrderDispatchStatus(context.Context, tenant.ID, string) (command.OrderDispatchStatus, error) {
	return f.status, f.err
}

// fakeStock answers Inventory's question "is a batch this order reserved past its printed
// expiry?" (the zero value: no) and counts how often it was asked.
type fakeStock struct {
	expired bool
	err     error
	asked   *int
}

func (f fakeStock) ReservedStockExpired(context.Context, tenant.ID, string) (bool, error) {
	if f.asked != nil {
		*f.asked++
	}
	return f.expired, f.err
}

func dispatch(t *testing.T, h command.MarkDispatchedHandler, tID tenant.ID, cnID consignmentnote.ID, actor membership.ID) error {
	t.Helper()
	return h.Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "BDX-1", TransitionedByMembership: actor,
	})
}

func requireStatus(t *testing.T, repo *consignmentnotetest.FakeRepository, tID tenant.ID, cnID consignmentnote.ID, want consignmentnote.Status) {
	t.Helper()
	got, err := repo.GetByID(t.Context(), tID, cnID)
	require.NoError(t, err)
	require.Equal(t, want, got.Status())
}

// Review G2d B1: goods do not leave without a tax invoice. A note whose order has no invoice
// stays pending.
func TestMarkDispatchedHandler_RefusesAnOrderWithoutAnInvoice(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkDispatchedHandler(repo, fakeOrder{status: command.OrderNotInvoiced}, fakeStock{}, fixedNow)

	require.ErrorIs(t, dispatch(t, h, tID, cnID, actor), command.ErrOrderNotInvoiced)
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
}

// Review G2e E2: an invoice is never removed, so a cancelled order still has one. The state of
// the order decides: only an invoiced or dispatched order is shipped.
func TestMarkDispatchedHandler_RefusesAnOrderInTheWrongState(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkDispatchedHandler(repo, fakeOrder{status: command.OrderWrongState}, fakeStock{}, fixedNow)

	err := dispatch(t, h, tID, cnID, actor)

	require.ErrorIs(t, err, command.ErrOrderNotDispatchable)
	require.NotErrorIs(t, err, command.ErrOrderNotInvoiced)
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
}

// Review G2e E1: the invoice can predate the expiry. While the note is pending the goods
// are still in the warehouse, so a batch the order reserved that is now past its printed
// month stops the dispatch.
func TestMarkDispatchedHandler_RefusesWhenAReservedBatchIsNowExpired(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	h := command.NewMarkDispatchedHandler(repo, fakeOrder{}, fakeStock{expired: true}, fixedNow)

	require.ErrorIs(t, dispatch(t, h, tID, cnID, actor), command.ErrStockExpired)
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
}

// Reads that cannot answer are not "fine": the dispatch fails closed, whichever read failed.
func TestMarkDispatchedHandler_FailsClosedWhenAReadCannotAnswer(t *testing.T) {
	t.Parallel()
	boom := errors.New("down")
	for name, build := range map[string]func(*consignmentnotetest.FakeRepository) command.MarkDispatchedHandler{
		"orders": func(r *consignmentnotetest.FakeRepository) command.MarkDispatchedHandler {
			return command.NewMarkDispatchedHandler(r, fakeOrder{err: boom}, fakeStock{}, fixedNow)
		},
		"inventory": func(r *consignmentnotetest.FakeRepository) command.MarkDispatchedHandler {
			return command.NewMarkDispatchedHandler(r, fakeOrder{}, fakeStock{err: boom}, fixedNow)
		},
	} {
		repo := consignmentnotetest.NewFakeRepository()
		tID, cnID, actor := seedPending(t, repo)

		err := dispatch(t, build(repo), tID, cnID, actor)

		require.ErrorIs(t, err, boom, name)
		require.NotErrorIs(t, err, command.ErrOrderNotInvoiced, name)
		requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
	}
}

// A note already past pending is not re-judged: a carrier that has the goods is not stopped
// by a month that ended since.
func TestMarkDispatchedHandler_ANotePastPendingIsNotRejudged(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	asked := 0
	ok := command.NewMarkDispatchedHandler(repo, fakeOrder{}, fakeStock{asked: &asked}, fixedNow)
	require.NoError(t, dispatch(t, ok, tID, cnID, actor))
	require.Equal(t, 1, asked)

	// The same docket again, after the month ended and the order was cancelled: nothing is asked.
	late := command.NewMarkDispatchedHandler(repo, fakeOrder{status: command.OrderWrongState}, fakeStock{expired: true, asked: &asked}, fixedNow)
	require.NoError(t, dispatch(t, late, tID, cnID, actor))
	require.Equal(t, 1, asked, "a note that is no longer pending is not asked about again")
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusDispatched)
}
