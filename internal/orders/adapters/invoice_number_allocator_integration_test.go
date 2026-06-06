//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn; the
// allocator runs millisecond UPDATEs inside short UoW txs, no long scans.

package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// tenantCtx returns a context bound to tenantID so a TxScopeTenant UoW tx
// sets app.tenant_id (RLS) when it opens.
func tenantCtx(t *testing.T, tid tenant.ID) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
}

// allocate runs one allocation inside a fresh tenant-scoped UoW tx. The UoW
// stashes the pgx.Tx in ctx, which is what the allocator reads.
func allocate(t *testing.T, tx *pg.Transactor, tid tenant.ID, fy invoicenumber.FinancialYear, kind invoicenumber.Kind) invoicenumber.Number {
	t.Helper()
	alloc := adapters.NewInvoiceNumberAllocator(sharedPG.Pool())
	var got invoicenumber.Number
	err := tx.WithinTx(tenantCtx(t, tid), pg.TxScopeTenant, func(ctx context.Context) error {
		n, aerr := alloc.Allocate(ctx, tid, fy, kind)
		got = n
		return aerr
	})
	require.NoError(t, err)
	return got
}

// TestInvoiceNumberAllocator_Sequential pins the core gapless contract:
// successive allocations for one (tenant, fy, kind) return 1, 2, 3 … and the
// display string is the canonical INV/2026-27/00001 form.
func TestInvoiceNumberAllocator_Sequential(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	tid := tenant.ID(uuid.NewString())
	fy := invoicenumber.FinancialYear("2026-27")

	n1 := allocate(t, tx, tid, fy, invoicenumber.KindInvoice)
	n2 := allocate(t, tx, tid, fy, invoicenumber.KindInvoice)
	n3 := allocate(t, tx, tid, fy, invoicenumber.KindInvoice)

	require.Equal(t, int64(1), n1.Seq())
	require.Equal(t, int64(2), n2.Seq())
	require.Equal(t, int64(3), n3.Seq())
	require.Equal(t, "INV/2026-27/00001", n1.String())
	require.Equal(t, "INV/2026-27/00003", n3.String())
}

// TestInvoiceNumberAllocator_IndependentPerKindFYTenant asserts each
// (tenant, fy, kind) triple owns an independent counter.
func TestInvoiceNumberAllocator_IndependentPerKindFYTenant(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	tid := tenant.ID(uuid.NewString())
	other := tenant.ID(uuid.NewString())
	fy := invoicenumber.FinancialYear("2026-27")
	fyNext := invoicenumber.FinancialYear("2027-28")

	require.Equal(t, int64(1), allocate(t, tx, tid, fy, invoicenumber.KindInvoice).Seq())
	require.Equal(t, int64(2), allocate(t, tx, tid, fy, invoicenumber.KindInvoice).Seq())

	// Different kind, same tenant/fy → own counter starting at 1.
	require.Equal(t, int64(1), allocate(t, tx, tid, fy, invoicenumber.KindCreditNote).Seq())
	// Different FY → own counter.
	require.Equal(t, int64(1), allocate(t, tx, tid, fyNext, invoicenumber.KindInvoice).Seq())
	// Different tenant → own counter.
	require.Equal(t, int64(1), allocate(t, tx, other, fy, invoicenumber.KindInvoice).Seq())
}

// TestInvoiceNumberAllocator_RollbackIsGapless asserts a rolled-back tx rolls
// back the increment, so the NEXT successful allocation reuses the number —
// gaplessness survives failure (ADR 0063 §3).
func TestInvoiceNumberAllocator_RollbackIsGapless(t *testing.T) {
	tx := pg.NewTransactor(ordersPool(t))
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	tid := tenant.ID(uuid.NewString())
	fy := invoicenumber.FinancialYear("2026-27")

	// First allocation commits → seq 1.
	require.Equal(t, int64(1), allocate(t, tx, tid, fy, invoicenumber.KindInvoice).Seq())

	// Second allocation increments to 2 but the tx rolls back.
	sentinel := errors.New("forced rollback")
	var rolledBackSeq int64
	err := tx.WithinTx(tenantCtx(t, tid), pg.TxScopeTenant, func(ctx context.Context) error {
		n, aerr := alloc.Allocate(ctx, tid, fy, invoicenumber.KindInvoice)
		require.NoError(t, aerr)
		rolledBackSeq = n.Seq()
		return sentinel // force rollback
	})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, int64(2), rolledBackSeq) // saw 2 inside the doomed tx

	// Next committed allocation reuses 2 — no gap.
	require.Equal(t, int64(2), allocate(t, tx, tid, fy, invoicenumber.KindInvoice).Seq())
}

// TestInvoiceNumberAllocator_RequiresTx asserts Allocate refuses to run
// outside a UoW tx (allocating without a tx would leak numbers on rollback).
func TestInvoiceNumberAllocator_RequiresTx(t *testing.T) {
	alloc := adapters.NewInvoiceNumberAllocator(ordersPool(t))
	_, err := alloc.Allocate(t.Context(), tenant.ID(uuid.NewString()), invoicenumber.FinancialYear("2026-27"), invoicenumber.KindInvoice)
	require.ErrorIs(t, err, adapters.ErrNoTx)
}
