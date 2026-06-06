// Package adapters holds the Orders module's outbound adapters per ADR 0002:
// pgx/sqlc-backed aggregate repositories, the gapless invoice-number
// allocator, and the outbox writer. Concrete (non-interface) types — domain
// consumers in internal/orders/app depend on the interfaces declared beside
// them in internal/orders/domain/*.
//
// All pgtype⇄scalar conversion goes through internal/common/pgconv (ADR 0066).
package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters/db"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// InvoiceNumberAllocator is the pgx/sqlc-backed [invoicenumber.Allocator].
// It MUST run inside the order's UoW tx (carried on ctx via
// [pg.TxFromContext]); the increment is part of that tx, so a rollback rolls
// the number back and the sequence stays gapless (ADR 0063 §3).
type InvoiceNumberAllocator struct {
	q *db.Queries
}

// NewInvoiceNumberAllocator wires the allocator.
func NewInvoiceNumberAllocator(pool *pgxpool.Pool) *InvoiceNumberAllocator {
	return &InvoiceNumberAllocator{q: db.New(pool)}
}

var _ invoicenumber.Allocator = (*InvoiceNumberAllocator)(nil)

// ErrNoTx is returned when Allocate runs without a surrounding UoW tx on ctx —
// allocating outside a tx would leak a number on any later rollback.
var ErrNoTx = errors.New("orders: invoice-number allocation must run inside a UoW tx")

// Allocate returns the next gapless number for (tenant, fy, kind). It lazily
// creates the sequence row, then atomically increments + returns last_used,
// both inside the ctx tx.
func (a *InvoiceNumberAllocator) Allocate(
	ctx context.Context, tenantID tenant.ID, fy invoicenumber.FinancialYear, kind invoicenumber.Kind,
) (invoicenumber.Number, error) {
	tx, ok := pg.TxFromContext(ctx)
	if !ok {
		return invoicenumber.Number{}, ErrNoTx
	}
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return invoicenumber.Number{}, fmt.Errorf("orders: parse tenant id %q: %w", tenantID, err)
	}
	q := a.q.WithTx(tx)
	if err := q.EnsureInvoiceNumberSequence(ctx, db.EnsureInvoiceNumberSequenceParams{
		TenantID:      pgconv.PgUUID(tid),
		FinancialYear: fy.String(),
		Kind:          kind.String(),
	}); err != nil {
		return invoicenumber.Number{}, fmt.Errorf("orders: ensure invoice-number sequence: %w", err)
	}
	seq, err := q.AllocateInvoiceNumber(ctx, db.AllocateInvoiceNumberParams{
		TenantID:      pgconv.PgUUID(tid),
		FinancialYear: fy.String(),
		Kind:          kind.String(),
	})
	if err != nil {
		return invoicenumber.Number{}, fmt.Errorf("orders: allocate invoice number: %w", err)
	}
	return invoicenumber.New(kind, fy, seq)
}
