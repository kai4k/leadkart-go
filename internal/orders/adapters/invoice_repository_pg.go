package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters/db"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// InvoiceRepository is the pgx/sqlc-backed [invoice.Repository]. Append-only.
type InvoiceRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewInvoiceRepository wires the repository.
func NewInvoiceRepository(pool *pgxpool.Pool, tx *pg.Transactor) *InvoiceRepository {
	return &InvoiceRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ invoice.Repository = (*InvoiceRepository)(nil)

// Add inserts a new Invoice. A 23505 on the order partial-unique index maps to
// [invoice.ErrAlreadyExistsForOrder]. Joins a surrounding UoW tx when present.
func (r *InvoiceRepository) Add(ctx context.Context, inv *invoice.Invoice) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, inv)
	}
	return r.tx.WithinTxPgxTenant(ctx, inv.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, inv)
	})
}

func (r *InvoiceRepository) addOnTx(ctx context.Context, tx pgx.Tx, inv *invoice.Invoice) error {
	params, err := insertInvoiceParams(inv)
	if err != nil {
		return err
	}
	if err := r.q.WithTx(tx).InsertInvoice(ctx, params); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_orders_invoices_order" {
			return invoice.ErrAlreadyExistsForOrder
		}
		return fmt.Errorf("orders repo: insert invoice: %w", err)
	}
	return nil
}

// GetByID returns the invoice or [invoice.ErrNotFound], tenant-scoped.
func (r *InvoiceRepository) GetByID(ctx context.Context, tenantID tenant.ID, id invoice.ID) (*invoice.Invoice, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse invoice id %q: %w", id, err)
	}
	return r.getOne(ctx, tenantID, func(ctx context.Context, q *db.Queries) (db.OrdersInvoice, error) {
		return q.GetInvoiceByID(ctx, pgconv.PgUUID(lid))
	})
}

// GetByOrderID returns the order's invoice or [invoice.ErrNotFound].
func (r *InvoiceRepository) GetByOrderID(ctx context.Context, tenantID tenant.ID, orderID order.ID) (*invoice.Invoice, error) {
	oid, err := uuid.Parse(orderID.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse order id %q: %w", orderID, err)
	}
	return r.getOne(ctx, tenantID, func(ctx context.Context, q *db.Queries) (db.OrdersInvoice, error) {
		return q.GetInvoiceByOrderID(ctx, pgconv.PgUUID(oid))
	})
}

func (r *InvoiceRepository) getOne(
	ctx context.Context, tenantID tenant.ID,
	query func(context.Context, *db.Queries) (db.OrdersInvoice, error),
) (*invoice.Invoice, error) {
	var out *invoice.Invoice
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := query(ctx, r.q.WithTx(tx))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return invoice.ErrNotFound
			}
			return fmt.Errorf("orders repo: get invoice: %w", gerr)
		}
		hydrated, herr := invoiceRowToAggregate(row)
		if herr != nil {
			return herr
		}
		out = hydrated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- mappers --------------------------------------------------------------

func insertInvoiceParams(inv *invoice.Invoice) (db.InsertInvoiceParams, error) {
	iid, err := uuid.Parse(inv.ID().String())
	if err != nil {
		return db.InsertInvoiceParams{}, fmt.Errorf("orders repo: parse invoice id: %w", err)
	}
	tid, err := uuid.Parse(inv.TenantID().String())
	if err != nil {
		return db.InsertInvoiceParams{}, fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	oid, err := uuid.Parse(inv.OrderID().String())
	if err != nil {
		return db.InsertInvoiceParams{}, fmt.Errorf("orders repo: parse order id: %w", err)
	}
	items, err := marshalLineItems(inv.LineItems())
	if err != nil {
		return db.InsertInvoiceParams{}, err
	}
	taxLines, err := marshalTaxLines(inv.TaxLines())
	if err != nil {
		return db.InsertInvoiceParams{}, err
	}
	issuedBy, err := uuid.Parse(inv.IssuedByMembershipID().String())
	if err != nil {
		return db.InsertInvoiceParams{}, fmt.Errorf("orders repo: parse issued_by: %w", err)
	}
	n := inv.Number()
	return db.InsertInvoiceParams{
		ID:                   pgconv.PgUUID(iid),
		TenantID:             pgconv.PgUUID(tid),
		OrderID:              pgconv.PgUUID(oid),
		NumberKind:           n.Kind().String(),
		NumberFinancialYear:  n.FinancialYear().String(),
		NumberSeq:            n.Seq(),
		NumberDisplay:        n.String(),
		LineItems:            items,
		TaxLines:             taxLines,
		SubtotalPaise:        inv.SubtotalPaise(),
		TaxPaise:             inv.TaxPaise(),
		GrandTotalPaise:      inv.GrandTotalPaise(),
		IssuedAt:             pgconv.PgRequiredTimestamp(inv.IssuedAt()),
		IssuedByMembershipID: pgconv.PgUUID(issuedBy),
	}, nil
}

func invoiceRowToAggregate(row db.OrdersInvoice) (*invoice.Invoice, error) {
	items, err := unmarshalLineItems(row.LineItems)
	if err != nil {
		return nil, err
	}
	taxLines, err := unmarshalTaxLines(row.TaxLines)
	if err != nil {
		return nil, err
	}
	num, err := invoicenumber.New(invoicenumber.Kind(row.NumberKind), invoicenumber.FinancialYear(row.NumberFinancialYear), row.NumberSeq)
	if err != nil {
		return nil, fmt.Errorf("orders repo: rebuild invoice number: %w", err)
	}
	return invoice.UnmarshalFromDB(invoice.Snapshot{
		ID:                   invoice.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:             tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		OrderID:              order.ID(pgconv.UUIDFromPg(row.OrderID).String()),
		Number:               num,
		LineItems:            items,
		TaxLines:             taxLines,
		SubtotalPaise:        row.SubtotalPaise,
		TaxPaise:             row.TaxPaise,
		GrandTotalPaise:      row.GrandTotalPaise,
		IssuedAt:             pgconv.TimeFromPg(row.IssuedAt),
		IssuedByMembershipID: membership.ID(pgconv.UUIDFromPg(row.IssuedByMembershipID).String()),
	}), nil
}
