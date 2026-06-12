//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn; short
// tenant-scoped txs only.

package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// seedInvoiceGraph persists the order + invoice the credit-note FK chain
// requires (orders_init now FKs credit_notes→invoices→orders), returning the
// invoice ID. Everything runs in one tenant-scoped UoW.
func seedInvoiceGraph(t *testing.T, tx *pg.Transactor, tid tenant.ID, actor membership.ID) invoice.ID {
	t.Helper()
	orders := adapters.NewOrderRepository(ordersPool(t), tx)
	invoices := adapters.NewInvoiceRepository(ordersPool(t), tx)
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))

	o, err := order.New(order.NewInput{
		ID:                    order.ID(uuid.NewString()),
		TenantID:              tid,
		ApprovedQuotationID:   quotation.ID(uuid.NewString()),
		CustomerLeadID:        quotation.CustomerLeadID(uuid.NewString()),
		ConfirmedItems:        []quotation.LineItem{sampleLineItem()},
		CreatedByMembershipID: actor,
		Now:                   nowUTC(),
	})
	require.NoError(t, err)

	var invID invoice.ID
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	require.NoError(t, tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := orders.Add(ctx, o); err != nil {
			return err
		}
		num, err := alloc.Allocate(ctx, tid, invoicenumber.FromDate(nowUTC()), invoicenumber.KindInvoice)
		if err != nil {
			return err
		}
		inv, err := invoice.New(invoice.NewInput{
			ID: invoice.ID(uuid.NewString()), TenantID: tid, OrderID: o.ID(), Number: num,
			LineItems: o.ConfirmedItems(), SubtotalPaise: o.SubtotalPaise(),
			TaxPaise: o.TaxPaise(), GrandTotalPaise: o.GrandTotalPaise(),
			IssuedAt: nowUTC(), IssuedByMembershipID: actor,
		})
		if err != nil {
			return err
		}
		invID = inv.ID()
		return invoices.Add(ctx, inv)
	}))
	return invID
}

func mintNote(t *testing.T, tx *pg.Transactor, repo *adapters.CreditNoteRepository, tid tenant.ID, invID invoice.ID, kind invoicenumber.Kind, amount int64) error {
	t.Helper()
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	actor := membership.ID(uuid.NewString())
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	return tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		num, err := alloc.Allocate(ctx, tid, invoicenumber.FromDate(nowUTC()), kind)
		if err != nil {
			return err
		}
		cn, err := creditnote.New(creditnote.NewInput{
			ID: creditnote.ID(uuid.NewString()), TenantID: tid, InvoiceID: invID,
			Number: num, Kind: kind, Reason: "integration test reversal",
			AmountPaise: amount, IssuedAt: nowUTC(), IssuedByMembership: actor,
		})
		if err != nil {
			return err
		}
		return repo.Add(ctx, cn)
	})
}

// TestCreditNoteRepository_RoundTripAndCancellationUniqueness pins:
// (a) the FK chain accepts a real order→invoice→note graph;
// (b) the number components survive the round trip;
// (c) a SECOND cancellation note for the same invoice maps the 23505 on
//
//	uq_orders_credit_notes_cancellation to ErrCancellationAlreadyExists,
//	with the doomed tx's number allocation rolled back (gapless);
//
// (d) credit notes (returns) stack freely and list in issue order.
func TestCreditNoteRepository_RoundTripAndCancellationUniqueness(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	repo := adapters.NewCreditNoteRepository(ordersPool(t), tx)
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	invID := seedInvoiceGraph(t, tx, tid, actor)

	// (a)+(b) cancellation note round trip.
	require.NoError(t, mintNote(t, tx, repo, tid, invID, invoicenumber.KindCancellationNote, 100800))
	notes, err := repo.ListByInvoice(t.Context(), tid, invID)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Equal(t, invoicenumber.KindCancellationNote, notes[0].Kind())
	require.Equal(t, "CN/2026-27/00001", notes[0].Number().String())
	require.Equal(t, int64(100800), notes[0].AmountPaise())

	// (c) duplicate cancellation rejected with the domain sentinel.
	err = mintNote(t, tx, repo, tid, invID, invoicenumber.KindCancellationNote, 100800)
	require.ErrorIs(t, err, creditnote.ErrCancellationAlreadyExists)

	// (d) credit notes stack; gapless CN sequence reused the rolled-back 2.
	require.NoError(t, mintNote(t, tx, repo, tid, invID, invoicenumber.KindCreditNote, 5000))
	require.NoError(t, mintNote(t, tx, repo, tid, invID, invoicenumber.KindCreditNote, 7000))
	notes, err = repo.ListByInvoice(t.Context(), tid, invID)
	require.NoError(t, err)
	require.Len(t, notes, 3)
	cancelSeqAfterRollback, err := mintNoteSeqProbe(t, tx, tid)
	require.NoError(t, err)
	require.Equal(t, int64(2), cancelSeqAfterRollback, "rolled-back cancellation allocation must not gap the CN sequence")
}

// errRollbackProbe forces a rollback after observing the next sequence value.
var errRollbackProbe = errors.New("rollback probe")

// mintNoteSeqProbe allocates (and rolls back) the next cancellation-note
// number to observe the sequence position without consuming it.
func mintNoteSeqProbe(t *testing.T, tx *pg.Transactor, tid tenant.ID) (int64, error) {
	t.Helper()
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	var seq int64
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		n, err := alloc.Allocate(ctx, tid, invoicenumber.FromDate(nowUTC()), invoicenumber.KindCancellationNote)
		if err != nil {
			return err
		}
		seq = n.Seq()
		return errRollbackProbe
	})
	if errors.Is(err, errRollbackProbe) {
		return seq, nil
	}
	return 0, err
}
