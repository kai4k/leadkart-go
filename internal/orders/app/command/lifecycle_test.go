package command_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
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
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment/paymenttest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation/quotationtest"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// fakeUoW runs the closure directly — the fake repos need no real tx.
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

// fakeEnqueuer records enqueued integration events.
type fakeEnqueuer struct{ events []integrationevents.Event }

func (f *fakeEnqueuer) EnqueueInTx(_ context.Context, evs ...integrationevents.Event) error {
	f.events = append(f.events, evs...)
	return nil
}

func now() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

func qID() quotation.ID   { return quotation.ID(uuid.NewString()) }
func oID() order.ID       { return order.ID(uuid.NewString()) }
func invID() invoice.ID   { return invoice.ID(uuid.NewString()) }
func cnID() creditnote.ID { return creditnote.ID(uuid.NewString()) }
func payID() payment.ID   { return payment.ID(uuid.NewString()) }

func sampleItem() command.CreateQuotationLineItem {
	return command.CreateQuotationLineItem{
		ProductID: uuid.NewString(), SKU: "SKU-1", Description: "Item",
		Quantity: 10, UnitMrpPaise: 10000, UnitSalePaise: 9000, GstRateBps: 1200,
	}
}

// TestOrderFulfilmentLifecycle drives the full saga path through the command
// handlers against fakes: quote → approve(+order) → token → confirm → pack →
// invoice(gapless number) → dispatch → delivered → complete, asserting state +
// the enriched events the saga depends on.
func TestOrderFulfilmentLifecycle(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	ctx := t.Context()

	quotes := quotationtest.NewFakeRepository()
	orders := ordertest.NewFakeRepository()
	invoices := invoicetest.NewFakeRepository()
	payments := paymenttest.NewFakeRepository()
	alloc := invoicenumbertest.NewFakeAllocator()
	enq := &fakeEnqueuer{}
	uow := fakeUoW{}

	// Create + approve quotation → order seeded.
	createQuote := command.NewCreateQuotationHandler(quotes, now, qID)
	quoteID, err := createQuote.Handle(ctx, command.CreateQuotationCommand{
		TenantID: tid, CustomerLeadID: uuid.NewString(),
		Items: []command.CreateQuotationLineItem{sampleItem()}, CreatedByMembershipID: actor,
	})
	require.NoError(t, err)

	approve := command.NewApproveQuotationHandler(uow, quotes, orders, now, oID)
	appr, err := approve.Handle(ctx, command.ApproveQuotationCommand{
		TenantID: tid, QuotationID: quoteID, ApprovedByMembership: actor,
	})
	require.NoError(t, err)
	orderID := appr.OrderID

	// Token payment → token_paid.
	token := command.NewRecordTokenPaymentHandler(uow, orders, payments, now, payID)
	require.NoError(t, token.Handle(ctx, command.RecordTokenPaymentCommand{
		TenantID: tid, OrderID: orderID, Method: "upi", AmountPaise: 25000, RecordedByMembership: actor,
	}))

	// Confirm → confirmed + OrderConfirmedV1.
	confirm := command.NewConfirmOrderHandler(uow, orders, enq, now)
	require.NoError(t, confirm.Handle(ctx, command.ConfirmOrderCommand{
		TenantID: tid, OrderID: orderID, ConfirmedByMembership: actor,
	}))

	// Pack → packed + OrderPackedV1.
	pack := command.NewPackOrderHandler(uow, orders, enq, now)
	require.NoError(t, pack.Handle(ctx, command.PackOrderCommand{
		TenantID: tid, OrderID: orderID, CarrierName: "BlueDart", BoxCount: 2, WeightGrams: 1500, PackedByMembership: actor,
	}))

	// Invoice → invoiced + gapless number.
	invoiceOrder := command.NewInvoiceOrderHandler(uow, orders, invoices, alloc, now, invID)
	invRes, err := invoiceOrder.Handle(ctx, command.InvoiceOrderCommand{
		TenantID: tid, OrderID: orderID, IssuedByMembership: actor,
	})
	require.NoError(t, err)
	require.Equal(t, "INV/2026-27/00001", invRes.NumberDisplay)

	// Dispatch (saga) → dispatched.
	attach := command.NewAttachConsignmentHandler(orders, now)
	require.NoError(t, attach.Handle(ctx, command.AttachConsignmentCommand{
		TenantID: tid, OrderID: orderID, ConsignmentNoteID: uuid.NewString(), TransitionedByMembership: actor,
	}))

	// Delivered (saga) → delivered, then complete.
	deliver := command.NewMarkOrderDeliveredHandler(orders, now)
	require.NoError(t, deliver.Handle(ctx, command.MarkOrderDeliveredCommand{
		TenantID: tid, OrderID: orderID, TransitionedByMembership: actor,
	}))
	complete := command.NewCompleteOrderHandler(orders, now)
	require.NoError(t, complete.Handle(ctx, command.CompleteOrderCommand{
		TenantID: tid, OrderID: orderID, TransitionedByMembership: actor,
	}))

	final, err := orders.GetByID(ctx, tid, orderID)
	require.NoError(t, err)
	require.Equal(t, order.StateComplete, final.State())
	require.NotEmpty(t, final.InvoiceID())

	// The saga's enriched events were enqueued.
	require.True(t, hasEvent(enq.events, integrationevents.TopicOrderConfirmedV1))
	require.True(t, hasEvent(enq.events, integrationevents.TopicOrderPackedV1))
}

// TestCancelOrder cancels an approved order and confirms the terminal state.
func TestCancelOrder(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	ctx := t.Context()
	quotes := quotationtest.NewFakeRepository()
	orders := ordertest.NewFakeRepository()
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

	require.NoError(t, command.NewCancelOrderHandler(orders, now).Handle(ctx, command.CancelOrderCommand{
		TenantID: tid, OrderID: appr.OrderID, Reason: "customer withdrew", CancelledByMembership: actor,
	}))
	got, err := orders.GetByID(ctx, tid, appr.OrderID)
	require.NoError(t, err)
	require.Equal(t, order.StateCancelled, got.State())
}

// TestMintCreditNote mints a cancellation note with a gapless CN number.
func TestMintCreditNote(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	ctx := t.Context()
	creditNotes := creditnotetest.NewFakeRepository()
	alloc := invoicenumbertest.NewFakeAllocator()
	uow := fakeUoW{}

	res, err := command.NewMintCreditNoteHandler(uow, creditNotes, alloc, now, cnID).Handle(ctx, command.MintCreditNoteCommand{
		TenantID: tid, InvoiceID: invoice.ID(uuid.NewString()), Kind: "cancellation_note",
		Reason: "order cancelled post-invoice", AmountPaise: 100800, IssuedByMembership: membership.ID(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Equal(t, "CN/2026-27/00001", res.NumberDisplay)
}

// TestRecordPayment records a balance receipt + rejects a duplicate external
// reference (webhook idempotency).
func TestRecordPayment(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	ctx := t.Context()
	payments := paymenttest.NewFakeRepository()
	h := command.NewRecordPaymentHandler(payments, now, payID)

	res, err := h.Handle(ctx, command.RecordPaymentCommand{
		TenantID: tid, OrderID: order.ID(uuid.NewString()),
		Kind: "full", Method: "neft", AmountPaise: 75000,
		ExternalReference: "UTR-1", RecordedByMembership: membership.ID(uuid.NewString()),
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.PaymentID.String())

	_, dupErr := h.Handle(ctx, command.RecordPaymentCommand{
		TenantID: tid, OrderID: order.ID(uuid.NewString()),
		Kind: "full", Method: "neft", AmountPaise: 1,
		ExternalReference: "UTR-1", RecordedByMembership: membership.ID(uuid.NewString()),
	})
	require.ErrorIs(t, dupErr, payment.ErrAlreadyExistsForExternalReference)
}

func hasEvent(evs []integrationevents.Event, topic string) bool {
	for _, e := range evs {
		if e.Topic() == topic {
			return true
		}
	}
	return false
}
