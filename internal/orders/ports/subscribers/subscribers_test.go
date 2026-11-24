package subscribers_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchevents "github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote/creditnotetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice/invoicetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber/invoicenumbertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order/ordertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
	"github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// fakeStock answers the Inventory expiry question with a switchable answer.
type fakeStock struct{ expired bool }

func (g *fakeStock) HoldsExpiredStock(context.Context, tenant.ID, order.ID) (bool, error) {
	return g.expired, nil
}

type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

func now() time.Time { return time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC) }

func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixture is the full saga harness: fakes + every command handler + the four
// subscribers, plus one order seeded to `packed`.
type fixture struct {
	tenantID tenant.ID
	actor    membership.ID
	orderID  order.ID
	orders   *ordertest.FakeRepository
	invoices *invoicetest.FakeRepository
	notes    *creditnotetest.FakeRepository
	stock    *fakeStock

	autoInvoice *subscribers.AutoInvoiceSubscriber
	created     *subscribers.ConsignmentCreatedSubscriber
	delivered   *subscribers.ConsignmentDeliveredSubscriber
	cancelComp  *subscribers.CancelCompensationSubscriber
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		tenantID: tenant.ID(uuid.NewString()),
		actor:    membership.ID(uuid.NewString()),
		orders:   ordertest.NewFakeRepository(),
		invoices: invoicetest.NewFakeRepository(),
		notes:    creditnotetest.NewFakeRepository(),
		stock:    &fakeStock{},
	}
	alloc := invoicenumbertest.NewFakeAllocator()
	uow := fakeUoW{}
	newInvoiceID := func() invoice.ID { return invoice.ID(uuid.NewString()) }
	newNoteID := func() creditnote.ID { return creditnote.ID(uuid.NewString()) }

	invoiceOrder := command.NewInvoiceOrderHandler(uow, f.orders, f.invoices, alloc, f.stock, now, newInvoiceID)
	attach := command.NewAttachConsignmentHandler(f.orders)
	deliver := command.NewMarkOrderDeliveredHandler(f.orders, now)
	mint := command.NewMintCreditNoteHandler(uow, f.notes, f.invoices, f.orders, alloc, now, newNoteID)
	compensate := command.NewCompensateOrderCancellationHandler(f.invoices, f.notes, mint)

	f.autoInvoice = subscribers.NewAutoInvoiceSubscriber(invoiceOrder, silentLog())
	f.created = subscribers.NewConsignmentCreatedSubscriber(attach, silentLog())
	f.delivered = subscribers.NewConsignmentDeliveredSubscriber(attach, deliver, silentLog())
	f.cancelComp = subscribers.NewCancelCompensationSubscriber(compensate, silentLog())

	// Seed an order at `packed` — the state at which the saga races begin.
	o, err := order.New(order.NewInput{
		ID:                  order.ID(uuid.NewString()),
		TenantID:            f.tenantID,
		ApprovedQuotationID: quotation.ID(uuid.NewString()),
		CustomerLeadID:      quotation.CustomerLeadID(uuid.NewString()),
		ConfirmedItems: []quotation.LineItem{{
			ProductID: uuid.NewString(), SKU: "SKU-1", Quantity: 10,
			UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200,
		}},
		CreatedByMembershipID: f.actor,
		Now:                   now(),
	})
	require.NoError(t, err)
	require.NoError(t, o.RecordTokenPayment(f.actor, now()))
	require.NoError(t, o.Confirm(f.actor, now()))
	require.NoError(t, o.MarkPacked(f.actor, now()))
	o.PullEvents()
	require.NoError(t, f.orders.Add(t.Context(), o))
	f.orderID = o.ID()
	return f
}

func (f *fixture) orderState(t *testing.T) order.State {
	t.Helper()
	o, err := f.orders.GetByID(t.Context(), f.tenantID, f.orderID)
	require.NoError(t, err)
	return o.State()
}

func (f *fixture) createdEvent(cnID uuid.UUID) *dispatchevents.ConsignmentNoteCreatedV1 {
	return &dispatchevents.ConsignmentNoteCreatedV1{
		ConsignmentNoteID:     cnID,
		TenantIDClaim:         uuid.MustParse(f.tenantID.String()),
		OrderID:               uuid.MustParse(f.orderID.String()),
		CarrierName:           "BlueDart",
		BoxCount:              2,
		WeightGrams:           1500,
		CreatedByMembershipID: uuid.MustParse(f.actor.String()),
		OccurredAtUTC:         now(),
	}
}

func (f *fixture) deliveredEvent(cnID uuid.UUID) *dispatchevents.ConsignmentDeliveredV1 {
	return &dispatchevents.ConsignmentDeliveredV1{
		ConsignmentNoteID:        cnID,
		TenantIDClaim:            uuid.MustParse(f.tenantID.String()),
		OrderID:                  uuid.MustParse(f.orderID.String()),
		DeliveredAtUTC:           now(),
		TransitionedByMembership: uuid.MustParse(f.actor.String()),
		OccurredAtUTC:            now(),
	}
}

// invoiceAndDispatch takes the seeded order through the auto-invoice and the goods leaving.
func (f *fixture) invoiceAndDispatch(t *testing.T) {
	t.Helper()
	require.NoError(t, f.autoInvoice.Handle(t.Context(), &integrationevents.OrderPackedV1{
		OrderID: f.orderID.String(), TenantID: f.tenantID.String(),
		BoxCount: 2, WeightGrams: 1500, CarrierName: "BlueDart",
		PackedAtUTC: now(), PackedByMembershipID: f.actor.String(),
	}))
	require.NoError(t, command.NewDispatchOrderHandler(f.orders, now).Handle(t.Context(), command.DispatchOrderCommand{
		TenantID: f.tenantID, OrderID: f.orderID, TransitionedByMembership: f.actor,
	}))
}

// TestConsignmentCreated_OnlyLinksTheNote pins review G2g: the consignment-created event
// links the note and nothing else. The order is not invoiced by it (that is the
// auto-invoice's job) and is not dispatched by it (the goods have not left).
func TestConsignmentCreated_OnlyLinksTheNote(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cn := uuid.New()

	require.NoError(t, f.created.Handle(t.Context(), f.createdEvent(cn)))

	require.Equal(t, order.StatePacked, f.orderState(t))
	o, err := f.orders.GetByID(t.Context(), f.tenantID, f.orderID)
	require.NoError(t, err)
	require.Equal(t, cn.String(), o.ConsignmentNoteID())
	require.Nil(t, o.DispatchedAt())
	_, err = f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.Error(t, err, "the created event must not mint the invoice")
}

// TestConsignmentCreated_AfterAutoInvoiceKeepsTheOrderInvoiced: normal path, the order is
// invoiced and stays invoiced with the note linked.
func TestConsignmentCreated_AfterAutoInvoiceKeepsTheOrderInvoiced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	require.NoError(t, f.autoInvoice.Handle(t.Context(), &integrationevents.OrderPackedV1{
		OrderID: f.orderID.String(), TenantID: f.tenantID.String(),
		BoxCount: 2, WeightGrams: 1500, CarrierName: "BlueDart",
		PackedAtUTC: now(), PackedByMembershipID: f.actor.String(),
	}))
	require.Equal(t, order.StateInvoiced, f.orderState(t))

	require.NoError(t, f.created.Handle(t.Context(), f.createdEvent(uuid.New())))
	require.Equal(t, order.StateInvoiced, f.orderState(t))
}

// TestConsignmentCreated_ReplayAcks: redelivery of the same event after the
// order has moved on must ack (same consignment ⇒ domain no-op).
func TestConsignmentCreated_ReplayAcks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cn := uuid.New()
	require.NoError(t, f.created.Handle(t.Context(), f.createdEvent(cn)))
	f.invoiceAndDispatch(t)
	require.NoError(t, f.created.Handle(t.Context(), f.createdEvent(cn)), "replay must ack")
	require.Equal(t, order.StateDispatched, f.orderState(t))
}

// TestConsignmentDelivered_LinksAndDeliversADispatchedOrder: the delivered event of a note
// whose created event was lost still delivers the dispatched order.
func TestConsignmentDelivered_LinksAndDeliversADispatchedOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.invoiceAndDispatch(t)

	require.NoError(t, f.delivered.Handle(t.Context(), f.deliveredEvent(uuid.New())))

	require.Equal(t, order.StateDelivered, f.orderState(t))
}

// TestConsignmentDelivered_ForAnOrderThatNeverLeftIsAConflict: a delivered note can exist only
// for an order the dispatch transaction moved to dispatched. An order that is not dispatched
// is a genuine conflict (retry, then the dead letter for a person), and nothing is repaired
// from here: no invoice is minted, the order does not move.
func TestConsignmentDelivered_ForAnOrderThatNeverLeftIsAConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	err := f.delivered.Handle(t.Context(), f.deliveredEvent(uuid.New()))

	require.Error(t, err)
	require.Equal(t, order.StatePacked, f.orderState(t))
	_, ierr := f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.Error(t, ierr, "the delivered event must not mint the invoice")
}

// TestConsignmentDelivered_ReplayAcks: redelivery after delivery (and even
// after manual completion) must ack.
func TestConsignmentDelivered_ReplayAcks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cn := uuid.New()
	f.invoiceAndDispatch(t)
	require.NoError(t, f.delivered.Handle(t.Context(), f.deliveredEvent(cn)))
	require.NoError(t, f.delivered.Handle(t.Context(), f.deliveredEvent(cn)), "replay must ack")
	require.Equal(t, order.StateDelivered, f.orderState(t))
}

func (f *fixture) cancelledEvent(priorState order.State, reason string) *integrationevents.OrderCancelledV1 {
	return &integrationevents.OrderCancelledV1{
		OrderID:               f.orderID.String(),
		TenantID:              f.tenantID.String(),
		PriorState:            priorState.String(),
		Reason:                reason,
		CancelledAtUTC:        now(),
		CancelledByMembership: f.actor.String(),
	}
}

// TestCancelCompensation_MintsCancellationNote: an order cancelled at
// `invoiced` mints exactly one cancellation note for the invoice grand total;
// replays mint nothing further (one-per-invoice natural key).
func TestCancelCompensation_MintsCancellationNote(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Invoice the order so there is something to reverse.
	f.invoiceAndDispatch(t)
	inv, err := f.invoices.GetByOrderID(t.Context(), f.tenantID, f.orderID)
	require.NoError(t, err)

	evt := f.cancelledEvent(order.StateInvoiced, "customer withdrew")
	require.NoError(t, f.cancelComp.Handle(t.Context(), evt))

	notes, err := f.notes.ListByInvoice(t.Context(), f.tenantID, inv.ID())
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Equal(t, inv.GrandTotalPaise(), notes[0].AmountPaise())
	require.Equal(t, "CN/2026-27/00001", notes[0].Number().String())

	// Replay: acks, no second note.
	require.NoError(t, f.cancelComp.Handle(t.Context(), evt))
	notes, err = f.notes.ListByInvoice(t.Context(), f.tenantID, inv.ID())
	require.NoError(t, err)
	require.Len(t, notes, 1)
}

// TestCancelCompensation_NoReversalOutsideInvoicedWindow: pre-invoice cancels
// have nothing to reverse; post-delivery cancels are operator-driven returns.
// Both ack without minting.
func TestCancelCompensation_NoReversalOutsideInvoicedWindow(t *testing.T) {
	t.Parallel()
	for _, prior := range []order.State{order.StateTokenPaid, order.StateConfirmed, order.StatePacked, order.StateDelivered} {
		f := newFixture(t)
		require.NoError(t, f.cancelComp.Handle(t.Context(), f.cancelledEvent(prior, "changed mind")))
		require.Empty(t, f.notes.Store, "prior=%s must not mint", prior)
	}
}

// TestAutoInvoice_ReplayAcks: a second order_packed delivery hits the
// one-invoice-per-order natural key and acks.
func TestAutoInvoice_ReplayAcks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	evt := &integrationevents.OrderPackedV1{
		OrderID: f.orderID.String(), TenantID: f.tenantID.String(),
		BoxCount: 2, WeightGrams: 1500, CarrierName: "BlueDart",
		PackedAtUTC: now(), PackedByMembershipID: f.actor.String(),
	}
	require.NoError(t, f.autoInvoice.Handle(t.Context(), evt))
	require.NoError(t, f.autoInvoice.Handle(t.Context(), evt), "replay must ack")
	require.Equal(t, order.StateInvoiced, f.orderState(t))
}
