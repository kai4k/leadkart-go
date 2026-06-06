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
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// CreditNoteRepository is the pgx/sqlc-backed [creditnote.Repository].
// Append-only.
type CreditNoteRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewCreditNoteRepository wires the repository.
func NewCreditNoteRepository(pool *pgxpool.Pool, tx *pg.Transactor) *CreditNoteRepository {
	return &CreditNoteRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ creditnote.Repository = (*CreditNoteRepository)(nil)

// Add inserts a new CreditNote. A 23505 on the cancellation partial-unique
// index maps to [creditnote.ErrCancellationAlreadyExists].
func (r *CreditNoteRepository) Add(ctx context.Context, c *creditnote.CreditNote) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, c)
	}
	return r.tx.WithinTxPgxTenant(ctx, c.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, c)
	})
}

func (r *CreditNoteRepository) addOnTx(ctx context.Context, tx pgx.Tx, c *creditnote.CreditNote) error {
	params, err := insertCreditNoteParams(c)
	if err != nil {
		return err
	}
	if err := r.q.WithTx(tx).InsertCreditNote(ctx, params); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_orders_credit_notes_cancellation" {
			return creditnote.ErrCancellationAlreadyExists
		}
		return fmt.Errorf("orders repo: insert credit note: %w", err)
	}
	return nil
}

// GetByID returns the credit note or [creditnote.ErrNotFound].
func (r *CreditNoteRepository) GetByID(ctx context.Context, tenantID tenant.ID, id creditnote.ID) (*creditnote.CreditNote, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse credit note id %q: %w", id, err)
	}
	var out *creditnote.CreditNote
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetCreditNoteByID(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return creditnote.ErrNotFound
			}
			return fmt.Errorf("orders repo: get credit note: %w", gerr)
		}
		hydrated, herr := creditNoteRowToAggregate(row)
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

// ListByInvoice returns every credit note on the invoice in issue order.
func (r *CreditNoteRepository) ListByInvoice(ctx context.Context, tenantID tenant.ID, invoiceID invoice.ID) ([]*creditnote.CreditNote, error) {
	iid, err := uuid.Parse(invoiceID.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse invoice id %q: %w", invoiceID, err)
	}
	var out []*creditnote.CreditNote
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, gerr := r.q.WithTx(tx).ListCreditNotesByInvoice(ctx, pgconv.PgUUID(iid))
		if gerr != nil {
			return fmt.Errorf("orders repo: list credit notes: %w", gerr)
		}
		out = make([]*creditnote.CreditNote, 0, len(rows))
		for _, row := range rows {
			hydrated, herr := creditNoteRowToAggregate(row)
			if herr != nil {
				return herr
			}
			out = append(out, hydrated)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- mappers --------------------------------------------------------------

func insertCreditNoteParams(c *creditnote.CreditNote) (db.InsertCreditNoteParams, error) {
	cid, err := uuid.Parse(c.ID().String())
	if err != nil {
		return db.InsertCreditNoteParams{}, fmt.Errorf("orders repo: parse credit note id: %w", err)
	}
	tid, err := uuid.Parse(c.TenantID().String())
	if err != nil {
		return db.InsertCreditNoteParams{}, fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	invID, err := uuid.Parse(c.InvoiceID().String())
	if err != nil {
		return db.InsertCreditNoteParams{}, fmt.Errorf("orders repo: parse invoice id: %w", err)
	}
	issuedBy, err := uuid.Parse(c.IssuedByMembership().String())
	if err != nil {
		return db.InsertCreditNoteParams{}, fmt.Errorf("orders repo: parse issued_by: %w", err)
	}
	n := c.Number()
	return db.InsertCreditNoteParams{
		ID:                   pgconv.PgUUID(cid),
		TenantID:             pgconv.PgUUID(tid),
		InvoiceID:            pgconv.PgUUID(invID),
		NumberKind:           n.Kind().String(),
		NumberFinancialYear:  n.FinancialYear().String(),
		NumberSeq:            n.Seq(),
		NumberDisplay:        n.String(),
		Reason:               c.Reason(),
		AmountPaise:          c.AmountPaise(),
		IssuedAt:             pgconv.PgRequiredTimestamp(c.IssuedAt()),
		IssuedByMembershipID: pgconv.PgUUID(issuedBy),
	}, nil
}

func creditNoteRowToAggregate(row db.OrdersCreditNote) (*creditnote.CreditNote, error) {
	num, err := invoicenumber.New(invoicenumber.Kind(row.NumberKind), invoicenumber.FinancialYear(row.NumberFinancialYear), row.NumberSeq)
	if err != nil {
		return nil, fmt.Errorf("orders repo: rebuild credit note number: %w", err)
	}
	return creditnote.UnmarshalFromDB(creditnote.Snapshot{
		ID:                 creditnote.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:           tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		InvoiceID:          invoice.ID(pgconv.UUIDFromPg(row.InvoiceID).String()),
		Number:             num,
		Kind:               invoicenumber.Kind(row.NumberKind),
		Reason:             row.Reason,
		AmountPaise:        row.AmountPaise,
		IssuedAt:           pgconv.TimeFromPg(row.IssuedAt),
		IssuedByMembership: membership.ID(pgconv.UUIDFromPg(row.IssuedByMembershipID).String()),
	}), nil
}
