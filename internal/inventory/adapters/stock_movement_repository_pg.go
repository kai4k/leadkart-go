package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// StockMovementRepository is the pgx/sqlc-backed [stockmovement.Repository].
// Append-only (no UpdateByID). Joins the surrounding UoW tx so the
// multi-aggregate write (Batch UPDATE + Movement INSERT) is single-tx (ADR 0008).
type StockMovementRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewStockMovementRepository constructs a StockMovementRepository.
func NewStockMovementRepository(pool *pgxpool.Pool, tx *pg.Transactor) *StockMovementRepository {
	return &StockMovementRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [stockmovement.Repository]. Joins surrounding UoW tx;
// GUC bound from m.TenantID() (ADR 0062).
func (r *StockMovementRepository) Add(ctx context.Context, m *stockmovement.Movement) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, m)
	}
	return r.tx.WithinTxPgxTenant(ctx, m.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, m)
	})
}

func (r *StockMovementRepository) addOnTx(ctx context.Context, tx pgx.Tx, m *stockmovement.Movement) error {
	q := r.q.WithTx(tx)
	if err := insertMovementRow(ctx, q, m); err != nil {
		return err
	}
	return drainMovementEvents(ctx, tx, m)
}

// GetByID satisfies [stockmovement.Repository]. GUC bound from tenantID (ADR 0062).
func (r *StockMovementRepository) GetByID(ctx context.Context, tenantID tenant.ID, id stockmovement.ID) (*stockmovement.Movement, error) {
	var out *stockmovement.Movement
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		mid, perr := uuid.Parse(id.String())
		if perr != nil {
			return fmt.Errorf("movement repo: parse id: %w", perr)
		}
		row, err := q.GetStockMovementByID(ctx, pgconv.PgUUID(mid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return stockmovement.ErrNotFound
			}
			return fmt.Errorf("movement repo: get by id: %w", err)
		}
		out = rowToMovement(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByBatchPage satisfies [stockmovement.Repository]. Keyset on
// (occurred_at DESC, id DESC) via idx_movements_batch_keyset.
// GUC bound from tenantID (ADR 0062).
func (r *StockMovementRepository) ListByBatchPage(ctx context.Context, tenantID tenant.ID, batchID batch.ID, req stockmovement.PageRequest) (pagination.Page[*stockmovement.Movement], error) {
	bid, err := uuid.Parse(batchID.String())
	if err != nil {
		return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("movement repo: parse batch id: %w", err)
	}

	cursorOccurredAt := pgconv.PgRequiredTimestamp(maxCursorTime())
	cursorID := pgconv.PgUUID(maxCursorUUID())
	if req.Cursor.ID != "" {
		cursorOccurredAt = pgconv.PgRequiredTimestamp(req.Cursor.SortValue)
		uid, perr := uuid.Parse(req.Cursor.ID)
		if perr != nil {
			return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("movement repo: parse cursor id: %w", perr)
		}
		cursorID = pgconv.PgUUID(uid)
	}
	filterType := string(req.Filter.Type)

	var out []*stockmovement.Movement
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListMovementsByBatchPage(ctx, db.ListMovementsByBatchPageParams{
			BatchID:          pgconv.PgUUID(bid),
			CursorOccurredAt: cursorOccurredAt,
			CursorID:         cursorID,
			Type:             filterType,
			//nolint:gosec // pageSize clamped to MaxPageSize (200)
			Limit: int32(req.PageSize + 1),
		})
		if err != nil {
			return fmt.Errorf("movement repo: list page: %w", err)
		}
		for _, row := range rows {
			out = append(out, rowToMovement(row))
		}
		return nil
	})
	if err != nil {
		return pagination.Page[*stockmovement.Movement]{}, err
	}
	page := pagination.BuildPage(out, req.PageSize, func(m *stockmovement.Movement) pagination.Cursor {
		return pagination.Cursor{SortValue: m.OccurredAt(), ID: m.ID().String()}
	})
	if page.Items == nil {
		page.Items = []*stockmovement.Movement{}
	}
	return page, nil
}

// ----- Helpers ---------------------------------------------------------------

func insertMovementRow(ctx context.Context, q *db.Queries, m *stockmovement.Movement) error {
	mid, _ := uuid.Parse(m.ID().String())
	bid, _ := uuid.Parse(m.BatchID().String())
	pid, _ := uuid.Parse(m.ProductID().String())
	tid, _ := uuid.Parse(m.TenantID().String())
	aid, _ := uuid.Parse(m.ActorMembershipID().String())
	return q.InsertStockMovement(ctx, db.InsertStockMovementParams{
		ID:                  pgconv.PgUUID(mid),
		BatchID:             pgconv.PgUUID(bid),
		ProductID:           pgconv.PgUUID(pid),
		TenantID:            pgconv.PgUUID(tid),
		Type:                string(m.Type()),
		Quantity:            m.Quantity(),
		QuantityOnHandAfter: m.QuantityOnHandAfter(),
		Reason:              m.Reason(),
		ActorMembershipID:   pgconv.PgUUID(aid),
		SourceReference:     m.SourceReference(),
		OccurredAt:          pgconv.PgRequiredTimestamp(m.OccurredAt()),
	})
}

func drainMovementEvents(ctx context.Context, tx pgx.Tx, m *stockmovement.Movement) error {
	evs := m.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(m.TenantID().String())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("movement repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func rowToMovement(row db.InventoryStockMovement) *stockmovement.Movement {
	mid := stockmovement.ID(pgconv.UUIDFromPg(row.ID).String())
	bid := batch.ID(pgconv.UUIDFromPg(row.BatchID).String())
	pid := product.ID(pgconv.UUIDFromPg(row.ProductID).String())
	tid := tenant.ID(pgconv.UUIDFromPg(row.TenantID).String())
	aid := membership.ID(pgconv.UUIDFromPg(row.ActorMembershipID).String())
	return stockmovement.UnmarshalFromDB(stockmovement.Snapshot{
		ID:                  mid,
		BatchID:             bid,
		ProductID:           pid,
		TenantID:            tid,
		Type:                batch.MovementType(row.Type),
		Quantity:            row.Quantity,
		QuantityOnHandAfter: row.QuantityOnHandAfter,
		Reason:              row.Reason,
		ActorMembershipID:   aid,
		SourceReference:     row.SourceReference,
		OccurredAt:          pgconv.TimeFromPg(row.OccurredAt),
	})
}
