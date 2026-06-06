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
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
)

// PaymentRepository is the pgx/sqlc-backed [payment.Repository]. Append-only.
type PaymentRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPaymentRepository wires the repository.
func NewPaymentRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PaymentRepository {
	return &PaymentRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ payment.Repository = (*PaymentRepository)(nil)

// Add inserts a new Payment. A 23505 on the external-reference partial-unique
// index maps to [payment.ErrAlreadyExistsForExternalReference].
func (r *PaymentRepository) Add(ctx context.Context, p *payment.Payment) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, p)
	}
	return r.tx.WithinTxPgxTenant(ctx, p.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, p)
	})
}

func (r *PaymentRepository) addOnTx(ctx context.Context, tx pgx.Tx, p *payment.Payment) error {
	params, err := insertPaymentParams(p)
	if err != nil {
		return err
	}
	if err := r.q.WithTx(tx).InsertPayment(ctx, params); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_orders_payments_external_reference" {
			return payment.ErrAlreadyExistsForExternalReference
		}
		return fmt.Errorf("orders repo: insert payment: %w", err)
	}
	return nil
}

// GetByID returns the payment or [payment.ErrNotFound].
func (r *PaymentRepository) GetByID(ctx context.Context, tenantID tenant.ID, id payment.ID) (*payment.Payment, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse payment id %q: %w", id, err)
	}
	var out *payment.Payment
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetPaymentByID(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return payment.ErrNotFound
			}
			return fmt.Errorf("orders repo: get payment: %w", gerr)
		}
		hydrated, herr := paymentRowToAggregate(row)
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

// ListByOrder returns the order's payments in receipt order.
func (r *PaymentRepository) ListByOrder(ctx context.Context, tenantID tenant.ID, orderID order.ID) ([]*payment.Payment, error) {
	oid, err := uuid.Parse(orderID.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse order id %q: %w", orderID, err)
	}
	var out []*payment.Payment
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, gerr := r.q.WithTx(tx).ListPaymentsByOrder(ctx, pgconv.PgUUID(oid))
		if gerr != nil {
			return fmt.Errorf("orders repo: list payments: %w", gerr)
		}
		out = make([]*payment.Payment, 0, len(rows))
		for _, row := range rows {
			hydrated, herr := paymentRowToAggregate(row)
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

func insertPaymentParams(p *payment.Payment) (db.InsertPaymentParams, error) {
	pid, err := uuid.Parse(p.ID().String())
	if err != nil {
		return db.InsertPaymentParams{}, fmt.Errorf("orders repo: parse payment id: %w", err)
	}
	tid, err := uuid.Parse(p.TenantID().String())
	if err != nil {
		return db.InsertPaymentParams{}, fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	oid, err := uuid.Parse(p.OrderID().String())
	if err != nil {
		return db.InsertPaymentParams{}, fmt.Errorf("orders repo: parse order id: %w", err)
	}
	recordedBy, err := uuid.Parse(p.RecordedByMembership().String())
	if err != nil {
		return db.InsertPaymentParams{}, fmt.Errorf("orders repo: parse recorded_by: %w", err)
	}
	return db.InsertPaymentParams{
		ID:                     pgconv.PgUUID(pid),
		TenantID:               pgconv.PgUUID(tid),
		OrderID:                pgconv.PgUUID(oid),
		Kind:                   p.Kind().String(),
		Method:                 p.Method().String(),
		AmountPaise:            p.AmountPaise(),
		ExternalReference:      p.ExternalReference(),
		Notes:                  p.Notes(),
		ReceivedAt:             pgconv.PgRequiredTimestamp(p.ReceivedAt()),
		RecordedAt:             pgconv.PgRequiredTimestamp(p.RecordedAt()),
		RecordedByMembershipID: pgconv.PgUUID(recordedBy),
	}, nil
}

func paymentRowToAggregate(row db.OrdersPayment) (*payment.Payment, error) {
	return payment.UnmarshalFromDB(payment.Snapshot{
		ID:                   payment.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:             tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		OrderID:              order.ID(pgconv.UUIDFromPg(row.OrderID).String()),
		Kind:                 payment.Kind(row.Kind),
		Method:               payment.Method(row.Method),
		AmountPaise:          row.AmountPaise,
		ExternalReference:    row.ExternalReference,
		Notes:                row.Notes,
		ReceivedAt:           pgconv.TimeFromPg(row.ReceivedAt),
		RecordedAt:           pgconv.TimeFromPg(row.RecordedAt),
		RecordedByMembership: membership.ID(pgconv.UUIDFromPg(row.RecordedByMembershipID).String()),
	}), nil
}
