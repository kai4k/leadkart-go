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
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// QuotationRepository is the pgx/sqlc-backed [quotation.Repository].
// Tenant-scoped via RLS (the transactor binds app.tenant_id). Domain↔row
// mapping lives here; *db.Queries hold the SQL.
type QuotationRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewQuotationRepository wires the repository.
func NewQuotationRepository(pool *pgxpool.Pool, tx *pg.Transactor) *QuotationRepository {
	return &QuotationRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ quotation.Repository = (*QuotationRepository)(nil)

// Add persists a new Quotation + drains its events. Joins a surrounding UoW tx
// when ctx carries one; else opens its own under tenant scope.
func (r *QuotationRepository) Add(ctx context.Context, q *quotation.Quotation) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, q)
	}
	return r.tx.WithinTxPgxTenant(ctx, q.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, q)
	})
}

func (r *QuotationRepository) addOnTx(ctx context.Context, tx pgx.Tx, q *quotation.Quotation) error {
	params, err := insertQuotationParams(q)
	if err != nil {
		return err
	}
	if err := r.q.WithTx(tx).InsertQuotation(ctx, params); err != nil {
		return fmt.Errorf("orders repo: insert quotation: %w", err)
	}
	return drainQuotationEvents(ctx, tx, q)
}

// GetByID returns the quotation or [quotation.ErrNotFound], tenant-scoped.
func (r *QuotationRepository) GetByID(ctx context.Context, tenantID tenant.ID, id quotation.ID) (*quotation.Quotation, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("orders repo: parse quotation id %q: %w", id, err)
	}
	var out *quotation.Quotation
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetQuotationByID(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return quotation.ErrNotFound
			}
			return fmt.Errorf("orders repo: get quotation: %w", gerr)
		}
		hydrated, herr := quotationRowToAggregate(row)
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

// UpdateByID runs the UpdateFn against a row-locked quotation.
func (r *QuotationRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id quotation.ID,
	mutator func(*quotation.Quotation) (bool, error),
) error {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return fmt.Errorf("orders repo: parse quotation id %q: %w", id, err)
	}
	run := func(ctx context.Context, tx pgx.Tx) error {
		row, gerr := r.q.WithTx(tx).GetQuotationByIDForUpdate(ctx, pgconv.PgUUID(lid))
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return quotation.ErrNotFound
			}
			return fmt.Errorf("orders repo: lock quotation: %w", gerr)
		}
		agg, herr := quotationRowToAggregate(row)
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
		params, perr := updateQuotationParams(agg)
		if perr != nil {
			return perr
		}
		if err := r.q.WithTx(tx).UpdateQuotation(ctx, params); err != nil {
			return fmt.Errorf("orders repo: update quotation: %w", err)
		}
		return drainQuotationEvents(ctx, tx, agg)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), run)
}

// ----- mappers --------------------------------------------------------------

func insertQuotationParams(q *quotation.Quotation) (db.InsertQuotationParams, error) {
	qid, err := uuid.Parse(q.ID().String())
	if err != nil {
		return db.InsertQuotationParams{}, fmt.Errorf("orders repo: parse quotation id: %w", err)
	}
	tid, err := uuid.Parse(q.TenantID().String())
	if err != nil {
		return db.InsertQuotationParams{}, fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	lead, err := uuid.Parse(q.CustomerLeadID().String())
	if err != nil {
		return db.InsertQuotationParams{}, fmt.Errorf("orders repo: parse customer_lead_id: %w", err)
	}
	revisions, err := marshalRevisions(q.Revisions())
	if err != nil {
		return db.InsertQuotationParams{}, err
	}
	createdBy, err := uuid.Parse(q.CreatedByMembershipID().String())
	if err != nil {
		return db.InsertQuotationParams{}, fmt.Errorf("orders repo: parse created_by: %w", err)
	}
	return db.InsertQuotationParams{
		ID:                     pgconv.PgUUID(qid),
		TenantID:               pgconv.PgUUID(tid),
		CustomerLeadID:         pgconv.PgUUID(lead),
		State:                  q.State().String(),
		Revisions:              revisions,
		ApprovedAt:             pgconv.PgTimestampPtr(q.ApprovedAt()),
		ApprovedByMembershipID: pgUUIDFromMembershipPtr(q.ApprovedByMembershipID()),
		RejectedAt:             pgconv.PgTimestampPtr(q.RejectedAt()),
		RejectedByMembershipID: pgUUIDFromMembershipPtr(q.RejectedByMembershipID()),
		RejectionReason:        q.RejectionReason(),
		CreatedAt:              pgconv.PgRequiredTimestamp(q.CreatedAt()),
		CreatedByMembershipID:  pgconv.PgUUID(createdBy),
	}, nil
}

func updateQuotationParams(q *quotation.Quotation) (db.UpdateQuotationParams, error) {
	qid, err := uuid.Parse(q.ID().String())
	if err != nil {
		return db.UpdateQuotationParams{}, fmt.Errorf("orders repo: parse quotation id: %w", err)
	}
	revisions, err := marshalRevisions(q.Revisions())
	if err != nil {
		return db.UpdateQuotationParams{}, err
	}
	return db.UpdateQuotationParams{
		ID:                     pgconv.PgUUID(qid),
		State:                  q.State().String(),
		Revisions:              revisions,
		ApprovedAt:             pgconv.PgTimestampPtr(q.ApprovedAt()),
		ApprovedByMembershipID: pgUUIDFromMembershipPtr(q.ApprovedByMembershipID()),
		RejectedAt:             pgconv.PgTimestampPtr(q.RejectedAt()),
		RejectedByMembershipID: pgUUIDFromMembershipPtr(q.RejectedByMembershipID()),
		RejectionReason:        q.RejectionReason(),
	}, nil
}

func quotationRowToAggregate(row db.OrdersQuotation) (*quotation.Quotation, error) {
	revisions, err := unmarshalRevisions(row.Revisions)
	if err != nil {
		return nil, err
	}
	return quotation.UnmarshalFromDB(quotation.Snapshot{
		ID:                     quotation.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:               tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		CustomerLeadID:         quotation.CustomerLeadID(pgconv.UUIDFromPg(row.CustomerLeadID).String()),
		State:                  quotation.State(row.State),
		Revisions:              revisions,
		ApprovedAt:             pgconv.TimePtrFromPg(row.ApprovedAt),
		ApprovedByMembershipID: membershipPtrFromPg(row.ApprovedByMembershipID),
		RejectedAt:             pgconv.TimePtrFromPg(row.RejectedAt),
		RejectedByMembershipID: membershipPtrFromPg(row.RejectedByMembershipID),
		RejectionReason:        row.RejectionReason,
		CreatedAt:              pgconv.TimeFromPg(row.CreatedAt),
		CreatedByMembershipID:  membership.ID(pgconv.UUIDFromPg(row.CreatedByMembershipID).String()),
	}), nil
}

func drainQuotationEvents(ctx context.Context, tx pgx.Tx, q *quotation.Quotation) error {
	evs := q.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(q.TenantID().String())
	if err != nil {
		return fmt.Errorf("orders repo: parse tenant id: %w", err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	return drainEventsToOutbox(ctx, tx, tid, asAny)
}
