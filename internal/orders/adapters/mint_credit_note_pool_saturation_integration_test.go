//go:build integration

// arch-test:parallel-safe — own small pool on the shared pgtest container, fresh
// tenant_ids, RLS isolation.

package adapters_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
)

// TestMintCreditNote_AtPoolSaturation_NeverWaitsForASecondConnection pins the
// rule that a mint uses exactly ONE pooled connection: every read inside its
// transaction (the invoice, the notes already on it) goes through that
// transaction. A read that takes a second connection while the first is held
// deadlocks the pool as soon as the concurrent mints reach the pool size: every
// connection is held by a mint waiting for one more, nothing is ever released,
// and every other request of every tenant queues behind them until the callers
// give up. Here: a pool of 4, 24 mints at once over three invoices (so mints
// both queue on one order lock and run side by side), plus an unrelated read.
// All 24 must succeed and the read must answer, well inside the deadline.
func TestMintCreditNote_AtPoolSaturation_NeverWaitsForASecondConnection(t *testing.T) {
	t.Parallel()
	const (
		poolSize = 4
		invoices = 3
		perInv   = 8 // 24 mints, six times the pool
		deadline = 8 * time.Second
	)

	// Seed through the shared pool; mint through a small pool of its own.
	seedTx := pg.NewTransactor(ordersPool(t))
	tid := tenant.ID(uuid.NewString())
	actor := membership.ID(uuid.NewString())
	invIDs := make([]invoice.ID, invoices)
	for i := range invIDs {
		invIDs[i] = seedInvoiceGraph(t, seedTx, tid, actor)
	}
	otherTenant := tenant.ID(uuid.NewString())
	otherInv := seedInvoiceGraph(t, seedTx, otherTenant, actor)

	cfg, err := pgxpool.ParseConfig(sharedPG.AppDSN())
	require.NoError(t, err)
	cfg.MaxConns = poolSize
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tx := pg.NewTransactor(pool)
	invoiceRepo := adapters.NewInvoiceRepository(pool, tx)
	mint := command.NewMintCreditNoteHandler(
		tx, adapters.NewCreditNoteRepository(pool, tx), invoiceRepo, adapters.NewOrderRepository(pool, tx),
		adapters.NewInvoiceNumberAllocator(pool), nowUTC,
		func() creditnote.ID { return creditnote.ID(uuid.NewString()) },
	)

	ctx, cancel := context.WithTimeout(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), deadline)
	defer cancel()

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		errs    = make([]error, invoices*perInv)
		readErr error
		readDur time.Duration
	)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = mint.Handle(ctx, command.MintCreditNoteCommand{
				TenantID: tid, InvoiceID: invIDs[i%invoices], Kind: "credit_note",
				Reason: "pool saturation", AmountPaise: 1, IssuedByMembership: actor,
			})
		}()
	}
	// An unrelated read (another tenant, no transaction of its own to hold) on the
	// same pool, started with the burst.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		began := time.Now()
		_, readErr = invoiceRepo.GetByID(ctx, otherTenant, otherInv)
		readDur = time.Since(began)
	}()
	close(start)
	wg.Wait()

	failed := 0
	for _, e := range errs {
		if e != nil {
			failed++
			t.Logf("mint failed: %v", e)
		}
	}
	require.Zerof(t, failed, "%d of %d concurrent mints failed on a pool of %d: a mint must never wait for a second connection", failed, len(errs), poolSize)
	require.NoError(t, readErr, "an unrelated read must not be starved by the mints")
	require.Lessf(t, readDur, deadline/2, "an unrelated read took %s while the mints ran", readDur)

	// Every note landed: 8 per invoice.
	notes := adapters.NewCreditNoteRepository(ordersPool(t), seedTx)
	for _, id := range invIDs {
		got, lerr := notes.ListByInvoice(t.Context(), tid, id)
		require.NoError(t, lerr)
		require.Len(t, got, perInv)
	}
}
