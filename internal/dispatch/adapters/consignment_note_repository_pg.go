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
	"github.com/leadkart/leadkart-go/internal/dispatch/adapters/db"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ConsignmentNoteRepository is the pgx/sqlc-backed [consignmentnote.Repository].
// Tenant-scoped — every read + write binds app.tenant_id via the transactor so
// RLS gates the table. Domain↔row mapping lives here; *db.Queries hold the SQL.
type ConsignmentNoteRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewConsignmentNoteRepository wires the repository.
func NewConsignmentNoteRepository(pool *pgxpool.Pool, tx *pg.Transactor) *ConsignmentNoteRepository {
	return &ConsignmentNoteRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ consignmentnote.Repository = (*ConsignmentNoteRepository)(nil)

// Add persists a new pending note + drains its events, under tenant scope.
// Joins a surrounding UoW tx when ctx carries one. A UNIQUE(tenant_id,
// order_id) 23505 maps to [consignmentnote.ErrAlreadyExistsForOrder].
func (r *ConsignmentNoteRepository) Add(ctx context.Context, cn *consignmentnote.ConsignmentNote) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, cn)
	}
	return r.tx.WithinTxPgxTenant(ctx, cn.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, cn)
	})
}

func (r *ConsignmentNoteRepository) addOnTx(ctx context.Context, tx pgx.Tx, cn *consignmentnote.ConsignmentNote) error {
	q := r.q.WithTx(tx)
	if err := insertConsignmentNoteRow(ctx, q, cn); err != nil {
		return err
	}
	return drainConsignmentNoteEvents(ctx, tx, cn)
}

// GetByID returns the note or [consignmentnote.ErrNotFound], tenant-scoped.
func (r *ConsignmentNoteRepository) GetByID(ctx context.Context, tenantID tenant.ID, id consignmentnote.ID) (*consignmentnote.ConsignmentNote, error) {
	return r.getBy(ctx, tenantID, func(ctx context.Context, q *db.Queries) (db.DispatchConsignmentNote, error) {
		uid, err := uuid.Parse(id.String())
		if err != nil {
			return db.DispatchConsignmentNote{}, fmt.Errorf("consignment note repo: parse id %q: %w", id, err)
		}
		return q.GetConsignmentNoteByID(ctx, pgconv.PgUUID(uid))
	})
}

// GetByOrderID returns the (zero or one) note for an order, tenant-scoped.
func (r *ConsignmentNoteRepository) GetByOrderID(ctx context.Context, tenantID tenant.ID, orderID consignmentnote.OrderID) (*consignmentnote.ConsignmentNote, error) {
	return r.getBy(ctx, tenantID, func(ctx context.Context, q *db.Queries) (db.DispatchConsignmentNote, error) {
		oid, err := uuid.Parse(orderID.String())
		if err != nil {
			return db.DispatchConsignmentNote{}, fmt.Errorf("consignment note repo: parse order_id %q: %w", orderID, err)
		}
		return q.GetConsignmentNoteByOrderID(ctx, pgconv.PgUUID(oid))
	})
}

// getBy runs a single-row lookup under tenant scope, joining a surrounding tx
// when present, and translates pgx.ErrNoRows to ErrNotFound.
func (r *ConsignmentNoteRepository) getBy(
	ctx context.Context,
	tenantID tenant.ID,
	query func(context.Context, *db.Queries) (db.DispatchConsignmentNote, error),
) (*consignmentnote.ConsignmentNote, error) {
	run := func(ctx context.Context, tx pgx.Tx) (*consignmentnote.ConsignmentNote, error) {
		row, err := query(ctx, r.q.WithTx(tx))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, consignmentnote.ErrNotFound
			}
			return nil, fmt.Errorf("consignment note repo: get: %w", err)
		}
		return rowToConsignmentNote(row), nil
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	var out *consignmentnote.ConsignmentNote
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := run(ctx, tx)
		out = got
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID runs the UpdateFn (TDL) under one tenant-scoped tx: row-lock the
// note, mutate, persist state (if shouldPersist), drain events. Joins a
// surrounding UoW tx when ctx carries one.
func (r *ConsignmentNoteRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id consignmentnote.ID,
	mutator func(*consignmentnote.ConsignmentNote) (bool, error),
) error {
	run := func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		uid, err := uuid.Parse(id.String())
		if err != nil {
			return fmt.Errorf("consignment note repo: parse id %q: %w", id, err)
		}
		row, err := q.GetConsignmentNoteByIDForUpdate(ctx, pgconv.PgUUID(uid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return consignmentnote.ErrNotFound
			}
			return fmt.Errorf("consignment note repo: get for update: %w", err)
		}
		cn := rowToConsignmentNote(row)
		shouldPersist, err := mutator(cn)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := updateConsignmentNoteRow(ctx, q, cn); err != nil {
			return err
		}
		return drainConsignmentNoteEvents(ctx, tx, cn)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), run)
}

// ----- Mappers --------------------------------------------------------------

func insertConsignmentNoteRow(ctx context.Context, q *db.Queries, cn *consignmentnote.ConsignmentNote) error {
	id, err := uuid.Parse(cn.ID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse id: %w", err)
	}
	tid, err := uuid.Parse(cn.TenantID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse tenant_id: %w", err)
	}
	oid, err := uuid.Parse(cn.OrderID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse order_id: %w", err)
	}
	createdBy, err := uuid.Parse(cn.CreatedByMembershipID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse created_by: %w", err)
	}
	err = q.InsertConsignmentNote(ctx, db.InsertConsignmentNoteParams{
		ID:                    pgconv.PgUUID(id),
		TenantID:              pgconv.PgUUID(tid),
		OrderID:               pgconv.PgUUID(oid),
		Status:                cn.Status().String(),
		CarrierName:           cn.CarrierName(),
		DocketNumber:          cn.DocketNumber(),
		BoxCount:              cn.BoxCount(),
		WeightGrams:           cn.WeightGrams(),
		ExpectedDeliveryAt:    pgconv.PgTimestampPtr(cn.ExpectedDeliveryAt()),
		DispatchedAt:          pgconv.PgTimestampPtr(cn.DispatchedAt()),
		InTransitAt:           pgconv.PgTimestampPtr(cn.InTransitAt()),
		DeliveredAt:           pgconv.PgTimestampPtr(cn.DeliveredAt()),
		FailedAt:              pgconv.PgTimestampPtr(cn.FailedAt()),
		FailureReason:         cn.FailureReason(),
		CreatedAt:             pgconv.PgRequiredTimestamp(cn.CreatedAt()),
		CreatedByMembershipID: pgconv.PgUUID(createdBy),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return consignmentnote.ErrAlreadyExistsForOrder
		}
		return fmt.Errorf("consignment note repo: insert: %w", err)
	}
	return nil
}

func updateConsignmentNoteRow(ctx context.Context, q *db.Queries, cn *consignmentnote.ConsignmentNote) error {
	id, err := uuid.Parse(cn.ID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse id: %w", err)
	}
	err = q.UpdateConsignmentNoteState(ctx, db.UpdateConsignmentNoteStateParams{
		ID:            pgconv.PgUUID(id),
		Status:        cn.Status().String(),
		DocketNumber:  cn.DocketNumber(),
		DispatchedAt:  pgconv.PgTimestampPtr(cn.DispatchedAt()),
		InTransitAt:   pgconv.PgTimestampPtr(cn.InTransitAt()),
		DeliveredAt:   pgconv.PgTimestampPtr(cn.DeliveredAt()),
		FailedAt:      pgconv.PgTimestampPtr(cn.FailedAt()),
		FailureReason: cn.FailureReason(),
	})
	if err != nil {
		return fmt.Errorf("consignment note repo: update: %w", err)
	}
	return nil
}

func rowToConsignmentNote(row db.DispatchConsignmentNote) *consignmentnote.ConsignmentNote {
	return consignmentnote.UnmarshalFromDB(consignmentnote.Snapshot{
		ID:                    consignmentnote.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:              tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		OrderID:               consignmentnote.OrderID(pgconv.UUIDFromPg(row.OrderID).String()),
		Status:                consignmentnote.Status(row.Status),
		CarrierName:           row.CarrierName,
		DocketNumber:          row.DocketNumber,
		BoxCount:              row.BoxCount,
		WeightGrams:           row.WeightGrams,
		ExpectedDeliveryAt:    pgconv.TimePtrFromPg(row.ExpectedDeliveryAt),
		DispatchedAt:          pgconv.TimePtrFromPg(row.DispatchedAt),
		InTransitAt:           pgconv.TimePtrFromPg(row.InTransitAt),
		DeliveredAt:           pgconv.TimePtrFromPg(row.DeliveredAt),
		FailedAt:              pgconv.TimePtrFromPg(row.FailedAt),
		FailureReason:         row.FailureReason,
		CreatedAt:             pgconv.TimeFromPg(row.CreatedAt),
		CreatedByMembershipID: membership.ID(pgconv.UUIDFromPg(row.CreatedByMembershipID).String()),
	})
}

// drainConsignmentNoteEvents maps the aggregate's domain events to integration
// events and writes them to the outbox in the same tx. The pure-domain mapper
// leaves DocketNumber (Dispatched) + Reason (Failed) blank because those live
// on the aggregate, not the StatusChanged event — enrich them here.
func drainConsignmentNoteEvents(ctx context.Context, tx pgx.Tx, cn *consignmentnote.ConsignmentNote) error {
	evs := cn.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(cn.TenantID().String())
	if err != nil {
		return fmt.Errorf("consignment note repo: parse tenant_id for outbox: %w", err)
	}
	mapped := make([]integrationevents.Event, 0, len(evs))
	for _, e := range evs {
		ie, err := integrationevents.FromDomainEvent(e)
		if err != nil {
			return fmt.Errorf("consignment note repo: map event: %w", err)
		}
		switch v := ie.(type) {
		case integrationevents.ConsignmentNoteDispatchedV1:
			v.DocketNumber = cn.DocketNumber()
			ie = v
		case integrationevents.ConsignmentNoteFailedV1:
			v.Reason = cn.FailureReason()
			ie = v
		}
		mapped = append(mapped, ie)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

// ----- helpers --------------------------------------------------------------

// isUniqueViolation reports whether err is a Postgres 23505. The only unique
// business constraint on consignment_notes is (tenant_id, order_id); a PK
// collision is an impossible UUIDv7 clash, so callers map 23505 to the
// order-duplicate sentinel.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pg.SQLStateUniqueViolation
}
