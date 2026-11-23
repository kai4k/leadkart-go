package command_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote/creditnotetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice/invoicetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber/invoicenumbertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order/ordertest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment/paymenttest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation/quotationtest"
)

// seedOrderRow stores a bare order, the row a credit note's invoice hangs off
// (the mint handler row-locks it to serialise notes on one invoice).
func seedOrderRow(t *testing.T, orders *ordertest.FakeRepository, tid tenant.ID) order.ID {
	t.Helper()
	actor := membership.ID(uuid.NewString())
	o, err := order.New(order.NewInput{
		ID: oID(), TenantID: tid, ApprovedQuotationID: quotation.ID(uuid.NewString()),
		CustomerLeadID: quotation.CustomerLeadID(uuid.NewString()),
		ConfirmedItems: []quotation.LineItem{{
			ProductID: uuid.NewString(), SKU: "S", Quantity: 1, UnitMrpPaise: 100, UnitSalePaise: 100, GstRateBps: 0,
		}},
		CreatedByMembershipID: actor, Now: now(),
	})
	require.NoError(t, err)
	require.NoError(t, orders.Add(t.Context(), o))
	return o.ID()
}

// seedInvoice stores an invoice (and its order) with the given grand total.
func seedInvoice(t *testing.T, invoices *invoicetest.FakeRepository, orders *ordertest.FakeRepository, alloc *invoicenumbertest.FakeAllocator, tid tenant.ID, grandTotal int64) *invoice.Invoice {
	t.Helper()
	num, err := alloc.Allocate(t.Context(), tid, "2026-27", invoicenumber.KindInvoice)
	require.NoError(t, err)
	inv, err := invoice.New(invoice.NewInput{
		ID: invID(), TenantID: tid, OrderID: seedOrderRow(t, orders, tid), Number: num,
		LineItems: []quotation.LineItem{{
			ProductID: uuid.NewString(), SKU: "S", Quantity: 1, UnitSalePaise: grandTotal, GstRateBps: 0,
		}},
		SubtotalPaise: grandTotal, TaxPaise: 0, GrandTotalPaise: grandTotal,
		IssuedAt: now(), IssuedByMembershipID: membership.ID(uuid.NewString()),
	})
	require.NoError(t, err)
	require.NoError(t, invoices.Add(t.Context(), inv))
	return inv
}

// C12: credit notes are capped at the invoice total, counting the notes
// already issued; a refused note writes nothing.
func TestMintCreditNote_CappedAtInvoiceTotal(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	notes := creditnotetest.NewFakeRepository()
	invoices := invoicetest.NewFakeRepository()
	orders := ordertest.NewFakeRepository()
	alloc := invoicenumbertest.NewFakeAllocator()
	inv := seedInvoice(t, invoices, orders, alloc, tid, 100000)
	mint := command.NewMintCreditNoteHandler(fakeUoW{}, notes, invoices, orders, alloc, now, cnID)
	do := func(amount int64) error {
		_, err := mint.Handle(t.Context(), command.MintCreditNoteCommand{
			TenantID: tid, InvoiceID: inv.ID(), Kind: "credit_note", Reason: "return",
			AmountPaise: amount, IssuedByMembership: actor,
		})
		return err
	}

	require.ErrorIs(t, do(100001), creditnote.ErrExceedsInvoice, "one paise over the invoice")
	require.Empty(t, notes.Store, "a refused note must write nothing")

	require.NoError(t, do(60000))
	require.ErrorIs(t, do(40001), creditnote.ErrExceedsInvoice, "counts the note already issued")
	require.Len(t, notes.Store, 1)

	require.NoError(t, do(40000), "exactly the remainder is allowed")
	require.ErrorIs(t, do(1), creditnote.ErrExceedsInvoice, "nothing left")
	require.Len(t, notes.Store, 2)
}

// A credit note against an invoice that does not exist is not-found, not a
// database foreign-key failure.
func TestMintCreditNote_UnknownInvoice(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	mint := command.NewMintCreditNoteHandler(fakeUoW{}, creditnotetest.NewFakeRepository(),
		invoicetest.NewFakeRepository(), ordertest.NewFakeRepository(), invoicenumbertest.NewFakeAllocator(), now, cnID)
	_, err := mint.Handle(t.Context(), command.MintCreditNoteCommand{
		TenantID: tid, InvoiceID: invoice.ID(uuid.NewString()), Kind: "credit_note", Reason: "r",
		AmountPaise: 1, IssuedByMembership: membership.ID(uuid.NewString()),
	})
	require.ErrorIs(t, err, invoice.ErrNotFound)
}

// The cancellation note reverses what the invoice still stands for: with an
// earlier return on the invoice it mints the remainder instead of failing the
// cap (which would dead-letter the cancel saga), and a replay mints nothing.
func TestCompensateOrderCancellation_MintsRemainderAfterEarlierCredit(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	ctx := t.Context()
	notes := creditnotetest.NewFakeRepository()
	invoices := invoicetest.NewFakeRepository()
	orders := ordertest.NewFakeRepository()
	alloc := invoicenumbertest.NewFakeAllocator()
	inv := seedInvoice(t, invoices, orders, alloc, tid, 100000)
	mint := command.NewMintCreditNoteHandler(fakeUoW{}, notes, invoices, orders, alloc, now, cnID)
	_, err := mint.Handle(ctx, command.MintCreditNoteCommand{
		TenantID: tid, InvoiceID: inv.ID(), Kind: "credit_note", Reason: "return", AmountPaise: 30000, IssuedByMembership: actor,
	})
	require.NoError(t, err)

	h := command.NewCompensateOrderCancellationHandler(invoices, notes, mint)
	cmd := command.CompensateOrderCancellationCommand{
		TenantID: tid, OrderID: inv.OrderID(), PriorState: order.StateInvoiced, Reason: "withdrawn", IssuedByMembership: actor,
	}
	res, err := h.Handle(ctx, cmd)
	require.NoError(t, err)
	require.True(t, res.Minted)
	all, err := notes.ListByInvoice(ctx, tid, inv.ID())
	require.NoError(t, err)
	require.Equal(t, int64(0), creditnote.RemainingPaise(inv.GrandTotalPaise(), all), "invoice fully reversed")

	res, err = h.Handle(ctx, cmd)
	require.NoError(t, err)
	require.False(t, res.Minted, "replay mints nothing")
}

// I3: the cancel route accepts a reason of up to 1000 characters, and the
// compensation prepends "order cancelled: " (17). The composed reason must be
// clipped to the note's own limit, in characters, or the note is refused, the
// event dead-letters and the invoice is never reversed.
func TestCompensateOrderCancellation_LongCancelReasonStillReversesTheInvoice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"983 characters (always fitted)", strings.Repeat("r", 983)},
		{"984 characters", strings.Repeat("r", 984)},
		{"1000 characters", strings.Repeat("r", order.MaxCancellationReasonChars)},
		{"1000 Devanagari characters (3000 bytes)", strings.Repeat("र", order.MaxCancellationReasonChars)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tid := tenant.ID(uuid.NewString())
			actor := membership.ID(uuid.NewString())
			ctx := t.Context()
			notes := creditnotetest.NewFakeRepository()
			invoices := invoicetest.NewFakeRepository()
			orders := ordertest.NewFakeRepository()
			alloc := invoicenumbertest.NewFakeAllocator()
			inv := seedInvoice(t, invoices, orders, alloc, tid, 100000)
			mint := command.NewMintCreditNoteHandler(fakeUoW{}, notes, invoices, orders, alloc, now, cnID)

			res, err := command.NewCompensateOrderCancellationHandler(invoices, notes, mint).Handle(ctx, command.CompensateOrderCancellationCommand{
				TenantID: tid, OrderID: inv.OrderID(), PriorState: order.StateInvoiced, Reason: tc.reason, IssuedByMembership: actor,
			})
			require.NoError(t, err, "a cancel reason the cancel route accepted must never block the reversal")
			require.True(t, res.Minted)
			all, err := notes.ListByInvoice(ctx, tid, inv.ID())
			require.NoError(t, err)
			require.Len(t, all, 1)
			require.Equal(t, int64(0), creditnote.RemainingPaise(inv.GrandTotalPaise(), all), "invoice fully reversed")
			got := all[0].Reason()
			require.LessOrEqual(t, utf8.RuneCountInString(got), creditnote.MaxReasonChars)
			require.True(t, utf8.ValidString(got), "the clip must not cut a character in half")
			require.True(t, strings.HasPrefix(got, "order cancelled: "))
			want := []rune("order cancelled: " + tc.reason)
			if len(want) > creditnote.MaxReasonChars {
				want = want[:creditnote.MaxReasonChars]
			}
			require.Equal(t, string(want), got, "the reason is kept up to the limit, never dropped")
		})
	}
}

func approvedOrder(t *testing.T, orders *ordertest.FakeRepository, tid tenant.ID, actor membership.ID) order.ID {
	t.Helper()
	ctx := t.Context()
	quotes := quotationtest.NewFakeRepository()
	quoteID, err := command.NewCreateQuotationHandler(quotes, now, qID).Handle(ctx, command.CreateQuotationCommand{
		TenantID: tid, CustomerLeadID: uuid.NewString(),
		Items: []command.CreateQuotationLineItem{sampleItem()}, CreatedByMembershipID: actor,
	})
	require.NoError(t, err)
	appr, err := command.NewApproveQuotationHandler(fakeUoW{}, quotes, orders, now, oID).Handle(ctx, command.ApproveQuotationCommand{
		TenantID: tid, QuotationID: quoteID, ApprovedByMembership: actor,
	})
	require.NoError(t, err)
	return appr.OrderID
}

// C11: a repeated token payment is refused (409), not recorded twice. The
// order is advanced first, so the second call never reaches the payment insert.
func TestRecordTokenPayment_SecondIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	orders := ordertest.NewFakeRepository()
	payments := paymenttest.NewFakeRepository()
	orderID := approvedOrder(t, orders, tid, actor)
	h := command.NewRecordTokenPaymentHandler(fakeUoW{}, orders, payments, now, payID)
	cmd := command.RecordTokenPaymentCommand{
		TenantID: tid, OrderID: orderID, Method: "upi", AmountPaise: 25000, RecordedByMembership: actor,
	}

	require.NoError(t, h.Handle(t.Context(), cmd))
	err := h.Handle(t.Context(), cmd)

	require.ErrorIs(t, err, order.ErrTokenAlreadyRecorded)
	got, lerr := payments.ListByOrder(t.Context(), tid, orderID)
	require.NoError(t, lerr)
	require.Len(t, got, 1, "the refused second token must not add a payment row")
}

// C12: POST /payments cannot take a token receipt: it would write a row
// without advancing the order.
func TestRecordPayment_TokenKindIsRefused(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(uuid.NewString())
	payments := paymenttest.NewFakeRepository()
	h := command.NewRecordPaymentHandler(payments, now, payID)

	_, err := h.Handle(t.Context(), command.RecordPaymentCommand{
		TenantID: tid, OrderID: oID(), Kind: string(payment.KindToken), Method: "upi", AmountPaise: 100,
		RecordedByMembership: membership.ID(uuid.NewString()),
	})

	require.ErrorIs(t, err, command.ErrTokenPaymentRoute)
	require.Empty(t, payments.Store)
}
