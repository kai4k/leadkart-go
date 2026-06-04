package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/adapters/db"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// WorkItemRepository is the pgx/sqlc-backed implementation of
// [workitem.Repository]. Tenant-scoped — every read/write runs under
// [pg.TxScopeTenant] so the connection's `app.tenant_id` GUC binds
// before queries touch the table; Postgres RLS does the rest.
type WorkItemRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewWorkItemRepository wires the repository against pool + transactor.
func NewWorkItemRepository(pool *pgxpool.Pool, tx *pg.Transactor) *WorkItemRepository {
	return &WorkItemRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [workitem.Repository]. Joins a surrounding UnitOfWork
// tx when ctx carries one (per ADR 0047); otherwise opens its own tx
// under tenant scope bound from the aggregate's TenantID.
//
// Translates the partial-unique-index 23505 SQLSTATE to
// [workitem.ErrAlreadyExistsForSource] so subscriber idempotency
// surfaces cleanly.
func (r *WorkItemRepository) Add(ctx context.Context, w *workitem.WorkItem) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, w)
	}
	return r.tx.WithinTxPgxTenant(ctx, w.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, w)
	})
}

func (r *WorkItemRepository) addOnTx(ctx context.Context, tx pgx.Tx, w *workitem.WorkItem) error {
	q := r.q.WithTx(tx)
	if err := insertWorkItemRow(ctx, q, w); err != nil {
		return err
	}
	return drainWorkItemEvents(ctx, tx, w)
}

// UpdateByID satisfies [workitem.Repository] — TDL Sep 2024 UpdateFn
// pattern. Load → updateFn → persist (if shouldPersist) → drain events.
func (r *WorkItemRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id workitem.ID,
	updateFn func(*workitem.WorkItem) (bool, error),
) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *WorkItemRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id workitem.ID,
	updateFn func(*workitem.WorkItem) (bool, error),
) error {
	q := r.q.WithTx(tx)
	w, err := loadWorkItem(ctx, q, id)
	if err != nil {
		return err
	}
	persist, err := updateFn(w)
	if err != nil {
		return err
	}
	if !persist {
		_ = w.PullEvents()
		return nil
	}
	if err := persistWorkItemState(ctx, q, w); err != nil {
		return err
	}
	return drainWorkItemEvents(ctx, tx, w)
}

// GetByID satisfies [workitem.Repository]. Tenant-scoped read.
func (r *WorkItemRepository) GetByID(ctx context.Context, tenantID tenant.ID, id workitem.ID) (*workitem.WorkItem, error) {
	var out *workitem.WorkItem
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadWorkItem(ctx, q, id)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetOpenBySource satisfies [workitem.Repository] — subscriber-flow
// idempotency lookup.
func (r *WorkItemRepository) GetOpenBySource(ctx context.Context, tenantID tenant.ID, entityType, entityID string) (*workitem.WorkItem, error) {
	if entityType == "" || entityID == "" {
		return nil, workitem.ErrNotFound
	}
	var out *workitem.WorkItem
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetOpenWorkItemBySource(ctx, db.GetOpenWorkItemBySourceParams{
			SourceEntityType: pgconv.ZeroToNil(entityType),
			SourceEntityID:   pgconv.ZeroToNil(entityID),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return workitem.ErrNotFound
			}
			return fmt.Errorf("tasks repo: get open by source: %w", err)
		}
		hydrated, hErr := openBySourceRowToWorkItem(row)
		if hErr != nil {
			return hErr
		}
		out = hydrated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPage satisfies [workitem.Repository] — cursor-paginated list.
func (r *WorkItemRepository) ListPage(
	ctx context.Context,
	tenantID tenant.ID,
	filter workitem.ListFilter,
	cursor pagination.Cursor,
	pageSize int,
) (pagination.Page[*workitem.WorkItem], error) {
	clamped := pagination.ClampPageSize(pageSize)
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*workitem.WorkItem]{}, fmt.Errorf("tasks repo: parse tenant id %q: %w", tenantID, err)
	}

	params := db.ListWorkItemsPageParams{
		TenantID:     pgconv.PgUUID(tid),
		PageSize:     int32(clamped) + 1, //nolint:gosec // clamped ≤ 200
		State:        pgconv.ZeroToNil(filter.State.String()),
		Type:         pgconv.ZeroToNil(filter.Type.String()),
		Priority:     pgconv.ZeroToNil(filter.Priority.String()),
		Assignee:     pgUUIDOpt(filter.AssignedToMembershipID),
		SelfAssignee: pgUUIDOpt(filter.SelfFilter),
		BatchID:      pgUUIDOpt(filter.BatchID),
		DueBefore:    pgconv.PgTimestamp(filter.DueBefore),
		DueAfter:     pgconv.PgTimestamp(filter.DueAfter),
		CursorDueAt:  pgconv.PgTimestamp(cursor.SortValue),
		CursorID:     pgUUIDOpt(cursor.ID),
	}

	var rows []db.ListWorkItemsPageRow
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := q.ListWorkItemsPage(ctx, params)
		if err != nil {
			return fmt.Errorf("tasks repo: list page: %w", err)
		}
		rows = got
		return nil
	})
	if err != nil {
		return pagination.Page[*workitem.WorkItem]{}, err
	}

	hasMore := false
	if len(rows) > clamped {
		hasMore = true
		rows = rows[:clamped]
	}
	items := make([]*workitem.WorkItem, 0, len(rows))
	for _, row := range rows {
		hydrated, hErr := listRowToWorkItem(row)
		if hErr != nil {
			return pagination.Page[*workitem.WorkItem]{}, hErr
		}
		items = append(items, hydrated)
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = pagination.Encode(pagination.Cursor{SortValue: last.DueAt(), ID: last.ID().String()})
	}
	return pagination.Page[*workitem.WorkItem]{
		Items:      items,
		HasMore:    hasMore,
		NextCursor: next,
	}, nil
}

// ListOverdueCandidates satisfies [workitem.Repository] — cross-tenant
// scan for the periodic overdue-scan job. Platform scope.
func (r *WorkItemRepository) ListOverdueCandidates(ctx context.Context, tenantID tenant.ID, asOf time.Time, limit int) ([]*workitem.WorkItem, error) {
	if limit <= 0 {
		limit = 100
	}
	var tidParam pgtype.UUID
	if !tenantID.IsZero() {
		parsed, err := uuid.Parse(tenantID.String())
		if err != nil {
			return nil, fmt.Errorf("tasks repo: parse tenant id %q: %w", tenantID, err)
		}
		tidParam = pgconv.PgUUID(parsed)
	}
	var out []*workitem.WorkItem
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListOverdueCandidates(ctx, db.ListOverdueCandidatesParams{
			AsOf:     pgconv.PgRequiredTimestamp(asOf),
			TenantID: tidParam,
			RowLimit: int32(limit), //nolint:gosec // limit ≤ 100 typically
		})
		if err != nil {
			return fmt.Errorf("tasks repo: list overdue: %w", err)
		}
		out = make([]*workitem.WorkItem, 0, len(rows))
		for _, row := range rows {
			hydrated, hErr := overdueCandidateRowToWorkItem(row)
			if hErr != nil {
				return hErr
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

// ListPurgeCandidates satisfies [workitem.Repository] — cross-tenant
// scan for the daily purge job. Platform scope.
func (r *WorkItemRepository) ListPurgeCandidates(ctx context.Context, tenantID tenant.ID, before time.Time, limit int) ([]*workitem.WorkItem, error) {
	if limit <= 0 {
		limit = 500
	}
	var tidParam pgtype.UUID
	if !tenantID.IsZero() {
		parsed, err := uuid.Parse(tenantID.String())
		if err != nil {
			return nil, fmt.Errorf("tasks repo: parse tenant id %q: %w", tenantID, err)
		}
		tidParam = pgconv.PgUUID(parsed)
	}
	var out []*workitem.WorkItem
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListPurgeCandidates(ctx, db.ListPurgeCandidatesParams{
			Before:   pgconv.PgRequiredTimestamp(before),
			TenantID: tidParam,
			RowLimit: int32(limit), //nolint:gosec // limit ≤ 500
		})
		if err != nil {
			return fmt.Errorf("tasks repo: list purge candidates: %w", err)
		}
		out = make([]*workitem.WorkItem, 0, len(rows))
		for _, row := range rows {
			hydrated, hErr := purgeCandidateRowToWorkItem(row)
			if hErr != nil {
				return hErr
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

// DeleteByID satisfies [workitem.Repository]. Tenant-scoped soft-delete
// (sets is_deleted = true + deleted_at = now()). Joins a surrounding
// UnitOfWork tx when ctx carries one (per ADR 0047); otherwise opens its
// own tx under tenant scope.
func (r *WorkItemRepository) DeleteByID(ctx context.Context, tenantID tenant.ID, id workitem.ID) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.deleteOnTx(ctx, tx, id)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.deleteOnTx(ctx, tx, id)
	})
}

func (r *WorkItemRepository) deleteOnTx(ctx context.Context, tx pgx.Tx, id workitem.ID) error {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return fmt.Errorf("tasks repo: parse id %q: %w", id, err)
	}
	q := r.q.WithTx(tx)
	// Verify row exists in tenant before soft-deleting (so a missing
	// row returns ErrNotFound, not a silent no-op).
	if _, err := loadWorkItem(ctx, q, id); err != nil {
		return err
	}
	return q.SoftDeleteWorkItem(ctx, pgconv.PgUUID(lid))
}

// CountDashboard satisfies [workitem.Repository].
func (r *WorkItemRepository) CountDashboard(ctx context.Context, tenantID tenant.ID, visibleMembershipIDs []string, asOf time.Time) (workitem.DashboardCounts, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return workitem.DashboardCounts{}, fmt.Errorf("tasks repo: parse tenant id %q: %w", tenantID, err)
	}
	var visible []pgtype.UUID
	if len(visibleMembershipIDs) > 0 {
		visible = make([]pgtype.UUID, 0, len(visibleMembershipIDs))
		for _, raw := range visibleMembershipIDs {
			parsed, err := uuid.Parse(raw)
			if err != nil {
				return workitem.DashboardCounts{}, fmt.Errorf("tasks repo: parse visible membership id %q: %w", raw, err)
			}
			visible = append(visible, pgconv.PgUUID(parsed))
		}
	}

	var out workitem.DashboardCounts
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.DashboardCounts(ctx, db.DashboardCountsParams{
			TenantID:             pgconv.PgUUID(tid),
			AsOf:                 pgconv.PgRequiredTimestamp(asOf),
			VisibleMembershipIds: visible,
		})
		if err != nil {
			return fmt.Errorf("tasks repo: dashboard counts: %w", err)
		}
		out = workitem.DashboardCounts{
			Today:          int(row.Today),
			Upcoming:       int(row.Upcoming),
			Overdue:        int(row.Overdue),
			CompletedToday: int(row.CompletedToday),
			TotalPending:   int(row.TotalPending),
		}
		return nil
	})
	if err != nil {
		return workitem.DashboardCounts{}, err
	}
	return out, nil
}

// ----- Helpers --------------------------------------------------------------

func loadWorkItem(ctx context.Context, q *db.Queries, id workitem.ID) (*workitem.WorkItem, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("tasks repo: parse id %q: %w", id, err)
	}
	row, err := q.GetWorkItemByID(ctx, pgconv.PgUUID(lid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, workitem.ErrNotFound
		}
		return nil, fmt.Errorf("tasks repo: get by id: %w", err)
	}
	return getByIDRowToWorkItem(row)
}

func insertWorkItemRow(ctx context.Context, q *db.Queries, w *workitem.WorkItem) error {
	lid, err := uuid.Parse(w.ID().String())
	if err != nil {
		return fmt.Errorf("tasks repo: parse id %q: %w", w.ID(), err)
	}
	tid, err := uuid.Parse(w.TenantID().String())
	if err != nil {
		return fmt.Errorf("tasks repo: parse tenant id %q: %w", w.TenantID(), err)
	}
	src := w.Source()
	err = q.InsertWorkItem(ctx, db.InsertWorkItemParams{
		ID:                     pgconv.PgUUID(lid),
		TenantID:               pgconv.PgUUID(tid),
		Type:                   w.Type().String(),
		Priority:               w.Priority().String(),
		State:                  w.State().String(),
		Title:                  w.Title(),
		Description:            w.Description(),
		AssignedToMembershipID: pgUUIDOpt(w.AssignedToMembershipID()),
		AssignedByMembershipID: pgUUIDOpt(w.AssignedByMembershipID()),
		DueAt:                  pgconv.PgRequiredTimestamp(w.DueAt()),
		CompletedAt:            pgconv.PgTimestamp(w.CompletedAt()),
		CancelledAt:            pgconv.PgTimestamp(w.CancelledAt()),
		CancellationReason:     w.CancellationReason(),
		BatchID:                pgUUIDOpt(w.BatchID()),
		SourceModule:           src.Module,
		SourceEntityType:       pgconv.ZeroToNil(src.EntityType),
		SourceEntityID:         pgconv.ZeroToNil(src.EntityID),
		CreatedAt:              pgconv.PgRequiredTimestamp(w.CreatedAt()),
		CreatedByMembershipID:  pgUUIDOpt(w.CreatedByMembershipID()),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_tasks_source_open" {
			return workitem.ErrAlreadyExistsForSource
		}
		return fmt.Errorf("tasks repo: insert: %w", err)
	}
	return nil
}

func persistWorkItemState(ctx context.Context, q *db.Queries, w *workitem.WorkItem) error {
	lid, err := uuid.Parse(w.ID().String())
	if err != nil {
		return fmt.Errorf("tasks repo: parse id %q: %w", w.ID(), err)
	}
	return q.UpdateWorkItem(ctx, db.UpdateWorkItemParams{
		ID:                     pgconv.PgUUID(lid),
		Priority:               w.Priority().String(),
		State:                  w.State().String(),
		Title:                  w.Title(),
		Description:            w.Description(),
		AssignedToMembershipID: pgUUIDOpt(w.AssignedToMembershipID()),
		DueAt:                  pgconv.PgRequiredTimestamp(w.DueAt()),
		CompletedAt:            pgconv.PgTimestamp(w.CompletedAt()),
		CancelledAt:            pgconv.PgTimestamp(w.CancelledAt()),
		CancellationReason:     w.CancellationReason(),
	})
}

func drainWorkItemEvents(ctx context.Context, tx pgx.Tx, w *workitem.WorkItem) error {
	evs := w.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(w.TenantID().String())
	if err != nil {
		return fmt.Errorf("tasks repo: parse tenant id %q: %w", w.TenantID(), err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("tasks repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}
