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

// stepLog is the one place the fake unit of work and both fake steps write to, so a test can
// assert what ran, in what order and inside which transaction.
type stepLog struct{ events []string }

type txMarker struct{}

func (l *stepLog) add(e string) { l.events = append(l.events, e) }

// fakeTenantUoW runs the closure with a ctx marked as "the transaction" and logs begin/end.
type fakeTenantUoW struct{ log *stepLog }

func (u fakeTenantUoW) WithinTenantTx(ctx context.Context, _ string, fn func(context.Context) error) error {
	u.log.add("begin")
	err := fn(context.WithValue(ctx, txMarker{}, true))
	if err != nil {
		u.log.add("rollback")
		return err
	}
	u.log.add("commit")
	return nil
}

// fakeOrderStep is Orders' step: it logs, notes whether it ran inside the transaction, and
// answers err.
type fakeOrderStep struct {
	log *stepLog
	err error
}

func (f fakeOrderStep) MarkOrderDispatched(ctx context.Context, _ tenant.ID, _ string, _ membership.ID) error {
	f.log.add("order")
	if ctx.Value(txMarker{}) == nil {
		f.log.add("order-outside-tx")
	}
	return f.err
}

// fakeStockStep is Inventory's step, same shape.
type fakeStockStep struct {
	log *stepLog
	err error
}

func (f fakeStockStep) ShipOrderStock(ctx context.Context, _ tenant.ID, _ string, _ membership.ID) error {
	f.log.add("stock")
	if ctx.Value(txMarker{}) == nil {
		f.log.add("stock-outside-tx")
	}
	return f.err
}

func newDispatchHandler(repo *consignmentnotetest.FakeRepository, log *stepLog, orderErr, stockErr error) command.MarkDispatchedHandler {
	return command.NewMarkDispatchedHandler(fakeTenantUoW{log: log}, repo,
		fakeOrderStep{log: log, err: orderErr}, fakeStockStep{log: log, err: stockErr}, fixedNow)
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

// ADR 0072: the order and the stock move inside the note's own transaction, in that order,
// and the note moves last.
func TestMarkDispatchedHandler_OrderThenStockThenNoteInOneTransaction(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	log := &stepLog{}

	require.NoError(t, dispatch(t, newDispatchHandler(repo, log, nil, nil), tID, cnID, actor))

	require.Equal(t, []string{"begin", "order", "stock", "commit"}, log.events,
		"one transaction; both steps ran inside it, never outside")
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusDispatched)
}

// A refusal by either step ends the transaction in a rollback and the note stays pending; a
// refusal by the order step stops before the stock step is asked.
func TestMarkDispatchedHandler_AStepRefusalRollsBackAndLeavesTheNotePending(t *testing.T) {
	t.Parallel()
	boom := errors.New("down")
	for _, c := range []struct {
		name       string
		orderErr   error
		stockErr   error
		want       error
		wantEvents []string
	}{
		{"order has no invoice", command.ErrOrderNotInvoiced, nil, command.ErrOrderNotInvoiced, []string{"begin", "order", "rollback"}},
		{"order in the wrong state", command.ErrOrderNotDispatchable, nil, command.ErrOrderNotDispatchable, []string{"begin", "order", "rollback"}},
		{"stock expired", nil, command.ErrStockExpired, command.ErrStockExpired, []string{"begin", "order", "stock", "rollback"}},
		{"stock not held", nil, command.ErrStockNotHeld, command.ErrStockNotHeld, []string{"begin", "order", "stock", "rollback"}},
		{"orders read fails", boom, nil, boom, []string{"begin", "order", "rollback"}},
		{"inventory read fails", nil, boom, boom, []string{"begin", "order", "stock", "rollback"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := consignmentnotetest.NewFakeRepository()
			tID, cnID, actor := seedPending(t, repo)
			log := &stepLog{}

			err := dispatch(t, newDispatchHandler(repo, log, c.orderErr, c.stockErr), tID, cnID, actor)

			require.ErrorIs(t, err, c.want)
			require.Equal(t, c.wantEvents, log.events)
			requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
		})
	}
}

// A note already past pending is not judged again and moves nothing: the same docket is a
// success that touches neither the order nor the stock (no second deduction), another docket
// is the domain's refusal.
func TestMarkDispatchedHandler_ANotePastPendingTouchesNothing(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	first := &stepLog{}
	require.NoError(t, dispatch(t, newDispatchHandler(repo, first, nil, nil), tID, cnID, actor))

	// Same docket again, with steps that would refuse everything: nothing is asked.
	again := &stepLog{}
	require.NoError(t, dispatch(t, newDispatchHandler(repo, again, command.ErrOrderNotDispatchable, command.ErrStockExpired), tID, cnID, actor))
	require.Equal(t, []string{"begin", "commit"}, again.events, "no order step, no stock step")

	other := &stepLog{}
	err := newDispatchHandler(repo, other, nil, nil).Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "OTHER-DOCKET", TransitionedByMembership: actor,
	})
	require.Error(t, err)
	require.NotContains(t, other.events, "order")
	require.NotContains(t, other.events, "stock")
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusDispatched)
}

func TestMarkDispatchedHandler_UnknownNoteIsNotFoundAndMovesNothing(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, _, actor := seedPending(t, repo)
	log := &stepLog{}

	err := dispatch(t, newDispatchHandler(repo, log, nil, nil), tID, "no-such-note", actor)

	require.ErrorIs(t, err, consignmentnote.ErrNotFound)
	require.NotContains(t, log.events, "order")
	require.NotContains(t, log.events, "stock")
}

// A docket the note would refuse is refused before the order or the stock are touched.
func TestMarkDispatchedHandler_ABadDocketIsRefusedBeforeAnyStepRuns(t *testing.T) {
	t.Parallel()
	repo := consignmentnotetest.NewFakeRepository()
	tID, cnID, actor := seedPending(t, repo)
	log := &stepLog{}

	err := newDispatchHandler(repo, log, nil, nil).Handle(t.Context(), command.MarkDispatchedCommand{
		TenantID: tID, ConsignmentNoteID: cnID, DocketNumber: "  ", TransitionedByMembership: actor,
	})

	require.ErrorIs(t, err, consignmentnote.ErrInvalid)
	require.Equal(t, []string{"begin", "rollback"}, log.events, "neither the order step nor the stock step ran")
	requireStatus(t, repo, tID, cnID, consignmentnote.StatusPending)
}
