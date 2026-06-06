package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/adapters/db"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// OrderRepository is the pgx/sqlc-backed [order.Repository]. Tenant-scoped via
// RLS. Domain↔row mapping lives here.
type OrderRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewOrderRepository wires the repository.
func NewOrderRepository(pool *pgxpool.Pool, tx *pg.Transactor) *OrderRepository {
	return &OrderRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ order.Repository = (*OrderRepository)(nil)

// Add persists a new Order + drains its events. Joins a surrounding UoW tx
// when ctx carries one; else opens its own under tenant scope.
func (r *OrderRepository) Add(ctx context.Context, o *order.Order) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, o)
	}
	return r.tx.WithinTxPgxTenant(ctx, o.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, o)
	})
}

func (r *OrderRepository) addOnTx(ctx context.Context, tx pgx.Tx, o *order.Order) error {
	params, err := insertOrderParams(o)
	if err != nil {
		return err
	}
	if err := r.q.WithTx(tx).InsertOrder(ctx, params); err != nil {
		return fmt.Errorf("orders repo: insert order: %w", err)
	}
	return drainOrderEvents(ctx, tx, o)
}

// GetByID returns the order or [order.ErrNotFound], tenant-scoped.
func (r *OrderRepository) GetByID(ctx context.Context, tenantID tenant.ID, id order.ID) (*order.Order, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse order id %q: %w", id, err)
	}
	var out *order.Order
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetOrderByID(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return order.ErrNotFound
			}
			return fmt.Errorf("orders repo: get order: %w", gerr)
		}
		hydrated, herr := orderRowToAggregate(row)
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

// UpdateByID runs the UpdateFn against a row-locked order.
func (r *OrderRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id order.ID,
	mutator func(*order.Order) (bool, error),
) error {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return fmt.Errorf("orders repo: parse order id %q: %w", id, err)
	}
	run := func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetOrderByIDForUpdate(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return order.ErrNotFound
			}
			return fmt.Errorf("orders repo: lock order: %w", gerr)
		}
		agg, herr := orderRowToAggregate(row)
		if herr != nil {
			return herr
		}
		persist, merr := mutator(agg)
		if merr != nil {
			return merr
		}
		if !persist {
			_ = agg.PullEvents()
			return nil
		}
		params, perr := updateOrderParams(agg)
		if perr != nil {
			return perr
		}
		if err := r.q.WithTx(tx).UpdateOrder(ctx, params); err != nil {
			return fmt.Errorf("orders repo: update order: %w", err)
		}
		return drainOrderEvents(ctx, tx, agg)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), run)
}

// ----- mappers --------------------------------------------------------------

func insertOrderParams(o *order.Order) (db.InsertOrderParams, error) {
	oid, err := uuid.Parse(o.ID().String())
	if err != nil {
		return db.InsertOrderParams{}, fmt.Errorf("orders repo: parse order id: %w", err)
	}
	tid, err := uuid.Parse(o.TenantID().String())
	if err != nil {
		return db.InsertOrderParams{}, fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	quoteID, err := uuid.Parse(o.ApprovedQuotationID().String())
	if err != nil {
		return db.InsertOrderParams{}, fmt.Errorf("orders repo: parse approved_quotation_id: %w", err)
	}
	lead, err := uuid.Parse(o.CustomerLeadID().String())
	if err != nil {
		return db.InsertOrderParams{}, fmt.Errorf("orders repo: parse customer_lead_id: %w", err)
	}
	items, err := marshalLineItems(o.ConfirmedItems())
	if err != nil {
		return db.InsertOrderParams{}, err
	}
	createdBy, err := uuid.Parse(o.CreatedByMembershipID().String())
	if err != nil {
		return db.InsertOrderParams{}, fmt.Errorf("orders repo: parse created_by: %w", err)
	}
	return db.InsertOrderParams{
		ID:                    pgconv.PgUUID(oid),
		TenantID:              pgconv.PgUUID(tid),
		ApprovedQuotationID:   pgconv.PgUUID(quoteID),
		CustomerLeadID:        pgconv.PgUUID(lead),
		State:                 o.State().String(),
		ConfirmedItems:        items,
		SubtotalPaise:         o.SubtotalPaise(),
		TaxPaise:              o.TaxPaise(),
		GrandTotalPaise:       o.GrandTotalPaise(),
		InvoiceID:             pgUUIDFromStringOrNull(o.InvoiceID()),
		ConsignmentNoteID:     pgUUIDFromStringOrNull(o.ConsignmentNoteID()),
		ConfirmedAt:           pgconv.PgTimestampPtr(o.ConfirmedAt()),
		PackedAt:              pgconv.PgTimestampPtr(o.PackedAt()),
		InvoicedAt:            pgconv.PgTimestampPtr(o.InvoicedAt()),
		DispatchedAt:          pgconv.PgTimestampPtr(o.DispatchedAt()),
		DeliveredAt:           pgconv.PgTimestampPtr(o.DeliveredAt()),
		CompletedAt:           pgconv.PgTimestampPtr(o.CompletedAt()),
		CancelledAt:           pgconv.PgTimestampPtr(o.CancelledAt()),
		CancellationReason:    o.CancellationReason(),
		CreatedAt:             pgconv.PgRequiredTimestamp(o.CreatedAt()),
		CreatedByMembershipID: pgconv.PgUUID(createdBy),
	}, nil
}

func updateOrderParams(o *order.Order) (db.UpdateOrderParams, error) {
	oid, err := uuid.Parse(o.ID().String())
	if err != nil {
		return db.UpdateOrderParams{}, fmt.Errorf("orders repo: parse order id: %w", err)
	}
	return db.UpdateOrderParams{
		ID:                 pgconv.PgUUID(oid),
		State:              o.State().String(),
		InvoiceID:          pgUUIDFromStringOrNull(o.InvoiceID()),
		ConsignmentNoteID:  pgUUIDFromStringOrNull(o.ConsignmentNoteID()),
		ConfirmedAt:        pgconv.PgTimestampPtr(o.ConfirmedAt()),
		PackedAt:           pgconv.PgTimestampPtr(o.PackedAt()),
		InvoicedAt:         pgconv.PgTimestampPtr(o.InvoicedAt()),
		DispatchedAt:       pgconv.PgTimestampPtr(o.DispatchedAt()),
		DeliveredAt:        pgconv.PgTimestampPtr(o.DeliveredAt()),
		CompletedAt:        pgconv.PgTimestampPtr(o.CompletedAt()),
		CancelledAt:        pgconv.PgTimestampPtr(o.CancelledAt()),
		CancellationReason: o.CancellationReason(),
	}, nil
}

func orderRowToAggregate(row db.OrdersOrder) (*order.Order, error) {
	items, err := unmarshalLineItems(row.ConfirmedItems)
	if err != nil {
		return nil, err
	}
	return order.UnmarshalFromDB(order.Snapshot{
		ID:                    order.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:              tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		ApprovedQuotationID:   quotation.ID(pgconv.UUIDFromPg(row.ApprovedQuotationID).String()),
		CustomerLeadID:        quotation.CustomerLeadID(pgconv.UUIDFromPg(row.CustomerLeadID).String()),
		State:                 order.State(row.State),
		ConfirmedItems:        items,
		SubtotalPaise:         row.SubtotalPaise,
		TaxPaise:              row.TaxPaise,
		GrandTotalPaise:       row.GrandTotalPaise,
		InvoiceID:             stringFromPgUUID(row.InvoiceID),
		ConsignmentNoteID:     stringFromPgUUID(row.ConsignmentNoteID),
		ConfirmedAt:           pgconv.TimePtrFromPg(row.ConfirmedAt),
		PackedAt:              pgconv.TimePtrFromPg(row.PackedAt),
		InvoicedAt:            pgconv.TimePtrFromPg(row.InvoicedAt),
		DispatchedAt:          pgconv.TimePtrFromPg(row.DispatchedAt),
		DeliveredAt:           pgconv.TimePtrFromPg(row.DeliveredAt),
		CompletedAt:           pgconv.TimePtrFromPg(row.CompletedAt),
		CancelledAt:           pgconv.TimePtrFromPg(row.CancelledAt),
		CancellationReason:    row.CancellationReason,
		CreatedAt:             pgconv.TimeFromPg(row.CreatedAt),
		CreatedByMembershipID: membership.ID(pgconv.UUIDFromPg(row.CreatedByMembershipID).String()),
	}), nil
}

func drainOrderEvents(ctx context.Context, tx pgx.Tx, o *order.Order) error {
	evs := o.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(o.TenantID().String())
	if err != nil {
		return fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	return drainEventsToOutbox(ctx, tx, tid, asAny)
}
