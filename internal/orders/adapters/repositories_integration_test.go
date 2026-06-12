//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn; each
// case is a handful of short tenant-scoped txs, no long scans.

package adapters_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

func sampleLineItem() quotation.LineItem {
	return quotation.LineItem{
		ProductID:     uuid.NewString(),
		SKU:           "SKU-001",
		Description:   "Paracetamol 500mg",
		Quantity:      10,
		UnitMrpPaise:  10000,
		UnitSalePaise: 9000,
		GstRateBps:    1200,
	}
}

// TestQuotationRepository_RoundTrip pins JSONB revision survival through
// Add → GetByID.
func TestQuotationRepository_RoundTrip(t *testing.T) {
	repo := adapters.NewQuotationRepository(ordersPool(t), pg.NewTransactor(ordersPool(t)))
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	q, err := quotation.New(quotation.NewInput{
		ID:                    quotation.ID(uuid.NewString()),
		TenantID:              tid,
		CustomerLeadID:        quotation.CustomerLeadID(uuid.NewString()),
		InitialItems:          []quotation.LineItem{sampleLineItem()},
		InitialNote:           "first quote",
		CreatedByMembershipID: actor,
		Now:                   nowUTC(),
	})
	require.NoError(t, err)

	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	require.NoError(t, repo.Add(ctx, q))

	got, err := repo.GetByID(t.Context(), tid, q.ID())
	require.NoError(t, err)
	require.Equal(t, q.ID(), got.ID())
	require.Equal(t, quotation.StateDraft, got.State())
	require.Len(t, got.CurrentRevision().Items, 1)
	require.Equal(t, "SKU-001", got.CurrentRevision().Items[0].SKU)
	require.Equal(t, int32(10), got.CurrentRevision().Items[0].Quantity)
}

// newApprovedOrder builds + persists an Order (state quotation_approved).
func newApprovedOrder(t *testing.T, repo *adapters.OrderRepository, tid tenant.ID, actor membership.ID) *order.Order {
	t.Helper()
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
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	require.NoError(t, repo.Add(ctx, o))
	return o
}

// TestOrderRepository_RoundTripAndUpdate pins Add, JSONB item survival, and the
// UpdateFn lifecycle transition persisting through GetByID.
func TestOrderRepository_RoundTripAndUpdate(t *testing.T) {
	repo := adapters.NewOrderRepository(ordersPool(t), pg.NewTransactor(ordersPool(t)))
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	o := newApprovedOrder(t, repo, tid, actor)

	got, err := repo.GetByID(t.Context(), tid, o.ID())
	require.NoError(t, err)
	require.Equal(t, order.StateQuotationApproved, got.State())
	require.Len(t, got.ConfirmedItems(), 1)
	require.Positive(t, got.GrandTotalPaise())

	// Advance quotation_approved → token_paid via the UpdateFn.
	err = repo.UpdateByID(t.Context(), tid, o.ID(), func(ord *order.Order) (bool, error) {
		if terr := ord.RecordTokenPayment(actor, nowUTC()); terr != nil {
			return false, terr
		}
		return true, nil
	})
	require.NoError(t, err)

	got, err = repo.GetByID(t.Context(), tid, o.ID())
	require.NoError(t, err)
	require.Equal(t, order.StateTokenPaid, got.State())
}

// TestOrderRepository_CancelDrainsOutbox pins that Cancel persists + the
// CancelledEvent drains to the shared outbox without error (the wire mapping
// path), leaving the order terminal.
func TestOrderRepository_CancelDrainsOutbox(t *testing.T) {
	repo := adapters.NewOrderRepository(ordersPool(t), pg.NewTransactor(ordersPool(t)))
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	o := newApprovedOrder(t, repo, tid, actor)

	err := repo.UpdateByID(t.Context(), tid, o.ID(), func(ord *order.Order) (bool, error) {
		if cerr := ord.Cancel("customer withdrew", actor, nowUTC()); cerr != nil {
			return false, cerr
		}
		return true, nil
	})
	require.NoError(t, err)

	got, err := repo.GetByID(t.Context(), tid, o.ID())
	require.NoError(t, err)
	require.Equal(t, order.StateCancelled, got.State())
	require.Equal(t, "customer withdrew", got.CancellationReason())
}

// TestInvoiceRepository_AllocatedNumberRoundTrip pins the allocator→invoice
// path: a gapless number is allocated in-tx, the invoice persists, and
// GetByOrderID rebuilds the number; a second invoice for the order is rejected.
func TestInvoiceRepository_AllocatedNumberRoundTrip(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	repo := adapters.NewInvoiceRepository(ordersPool(t), tx)
	orderRepo := adapters.NewOrderRepository(ordersPool(t), tx)
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	// invoices.order_id now FKs orders.orders — seed the parent order.
	orderID := newApprovedOrder(t, orderRepo, tid, actor).ID()
	fy := invoicenumber.FromDate(nowUTC())

	var inv *invoice.Invoice
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		num, aerr := alloc.Allocate(ctx, tid, fy, invoicenumber.KindInvoice)
		if aerr != nil {
			return aerr
		}
		built, berr := invoice.New(invoice.NewInput{
			ID:                   invoice.ID(uuid.NewString()),
			TenantID:             tid,
			OrderID:              orderID,
			Number:               num,
			LineItems:            []quotation.LineItem{sampleLineItem()},
			SubtotalPaise:        90000,
			TaxPaise:             10800,
			GrandTotalPaise:      100800,
			IssuedAt:             nowUTC(),
			IssuedByMembershipID: actor,
		})
		if berr != nil {
			return berr
		}
		inv = built
		return repo.Add(ctx, built)
	})
	require.NoError(t, err)

	got, err := repo.GetByOrderID(t.Context(), tid, orderID)
	require.NoError(t, err)
	require.Equal(t, inv.ID(), got.ID())
	require.Equal(t, int64(1), got.Number().Seq())
	require.Equal(t, invoicenumber.KindInvoice, got.Number().Kind())

	// Second invoice for the same order is rejected.
	dupErr := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		num, aerr := alloc.Allocate(ctx, tid, fy, invoicenumber.KindInvoice)
		require.NoError(t, aerr)
		dup, berr := invoice.New(invoice.NewInput{
			ID: invoice.ID(uuid.NewString()), TenantID: tid, OrderID: orderID, Number: num,
			LineItems:     []quotation.LineItem{sampleLineItem()},
			SubtotalPaise: 1, TaxPaise: 0, GrandTotalPaise: 1, IssuedAt: nowUTC(), IssuedByMembershipID: actor,
		})
		require.NoError(t, berr)
		return repo.Add(ctx, dup)
	})
	require.ErrorIs(t, dupErr, invoice.ErrAlreadyExistsForOrder)
}

// TestPaymentRepository_AddListAndDuplicateRef pins append + list + the
// external-reference idempotency 23505 mapping.
func TestPaymentRepository_AddListAndDuplicateRef(t *testing.T) {
	repo := adapters.NewPaymentRepository(ordersPool(t), pg.NewTransactor(ordersPool(t)))
	tid := tenant.ID(uuid.NewString())
	orderID := order.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	ref := "UTR-" + uuid.NewString()

	mk := func(ref string) *payment.Payment {
		p, err := payment.New(payment.NewInput{
			ID:                   payment.ID(uuid.NewString()),
			TenantID:             tid,
			OrderID:              orderID,
			Kind:                 payment.KindToken,
			Method:               payment.MethodUPI,
			AmountPaise:          25000,
			ExternalReference:    ref,
			ReceivedAt:           nowUTC(),
			RecordedAt:           nowUTC(),
			RecordedByMembership: actor,
		})
		require.NoError(t, err)
		return p
	}

	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	require.NoError(t, repo.Add(ctx, mk(ref)))

	list, err := repo.ListByOrder(t.Context(), tid, orderID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, int64(25000), list[0].AmountPaise())

	// Same external reference → idempotency rejection.
	require.ErrorIs(t, repo.Add(ctx, mk(ref)), payment.ErrAlreadyExistsForExternalReference)
}
