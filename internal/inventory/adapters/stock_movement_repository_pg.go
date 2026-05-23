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
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// StockMovementRepository is the pgx/sqlc-backed implementation of
// [stockmovement.Repository]. Append-only — no UpdateByID. Joins the
// surrounding UoW tx so the multi-aggregate (Batch UPDATE + Movement
// INSERT) write is single-tx per ADR 0008.
type StockMovementRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewStockMovementRepository wires the repository.
func NewStockMovementRepository(pool *pgxpool.Pool, tx *pg.Transactor) *StockMovementRepository {
	return &StockMovementRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [stockmovement.Repository]. Joins surrounding UoW tx.
func (r *StockMovementRepository) Add(ctx context.Context, m *stockmovement.Movement) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, m)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
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

// GetByID satisfies [stockmovement.Repository].
func (r *StockMovementRepository) GetByID(ctx context.Context, id stockmovement.ID) (*stockmovement.Movement, error) {
	var out *stockmovement.Movement
	err := r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		mid, perr := uuid.Parse(id.String())
		if perr != nil {
			return fmt.Errorf("movement repo: parse id: %w", perr)
		}
		row, err := q.GetStockMovementByID(ctx, pgUUID(mid))
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
// (occurred_at DESC, id DESC) per migration index idx_movements_batch_keyset.
func (r *StockMovementRepository) ListByBatchPage(ctx context.Context, batchID batch.ID, req stockmovement.PageRequest) (pagination.Page[*stockmovement.Movement], error) {
	bid, err := uuid.Parse(batchID.String())
	if err != nil {
		return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("movement repo: parse batch id: %w", err)
	}

	cursorOccurredAt := pgRequiredTimestamp(maxCursorTime())
	cursorID := pgUUID(maxCursorUUID())
	if req.Cursor.ID != "" {
		cursorOccurredAt = pgRequiredTimestamp(req.Cursor.SortValue)
		uid, perr := uuid.Parse(req.Cursor.ID)
		if perr != nil {
			return pagination.Page[*stockmovement.Movement]{}, fmt.Errorf("movement repo: parse cursor id: %w", perr)
		}
		cursorID = pgUUID(uid)
	}
	filterType := string(req.Filter.Type)

	var out []*stockmovement.Movement
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListMovementsByBatchPage(ctx, db.ListMovementsByBatchPageParams{
			BatchID:          pgUUID(bid),
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
		ID:                  pgUUID(mid),
		BatchID:             pgUUID(bid),
		ProductID:           pgUUID(pid),
		TenantID:            pgUUID(tid),
		Type:                string(m.Type()),
		Quantity:            m.Quantity(),
		QuantityOnHandAfter: m.QuantityOnHandAfter(),
		Reason:              m.Reason(),
		ActorMembershipID:   pgUUID(aid),
		SourceReference:     m.SourceReference(),
		OccurredAt:          pgRequiredTimestamp(m.OccurredAt()),
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
	mid := stockmovement.ID(uuidFromPg(row.ID).String())
	bid := batch.ID(uuidFromPg(row.BatchID).String())
	pid := product.ID(uuidFromPg(row.ProductID).String())
	tid := tenant.ID(uuidFromPg(row.TenantID).String())
	aid := membership.ID(uuidFromPg(row.ActorMembershipID).String())
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
		OccurredAt:          timeFromPg(row.OccurredAt),
	})
}
