package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ReminderRepository is the pgx/sqlc-backed implementation of
// [reminder.Repository]. Tenant-scoped — every read + write runs under
// [pg.TxScopeTenant]; GUC bound from explicit tenantID per ADR 0062.
//
// Per ADR 0047, app/ code does NOT import this struct — handlers depend
// on [reminder.Repository] (the interface).
type ReminderRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewReminderRepository wires the repository against a pool + transactor.
func NewReminderRepository(pool *pgxpool.Pool, tx *pg.Transactor) *ReminderRepository {
	return &ReminderRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [reminder.Repository]. Drains events into the outbox in
// the same tx; joins a surrounding UoW when ctx carries one. The
// aggregate carries its own TenantID — the GUC is bound from
// r.TenantID() (TDL canon per ADR 0062).
//
// Returns [reminder.ErrAlreadyExists] when the partial unique indices
// fire (SQLSTATE 23505 on uq_crm_reminders_callback_pending or
// uq_crm_reminders_mature_pending). Subscriber + scheduler callers
// treat that branch as success (ACK).
func (r *ReminderRepository) Add(ctx context.Context, rem *reminder.Reminder) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, rem)
	}
	return r.tx.WithinTxPgxTenant(ctx, rem.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, rem)
	})
}

func (r *ReminderRepository) addOnTx(ctx context.Context, tx pgx.Tx, rem *reminder.Reminder) error {
	q := r.q.WithTx(tx)
	if err := insertReminderRow(ctx, q, rem); err != nil {
		return err
	}
	return drainReminderEvents(ctx, tx, rem)
}

// UpdateByID satisfies [reminder.Repository]. TDL Sep 2024 UpdateFn
// pattern. Load → updateFn → persist (if shouldPersist) → drain events.
// All in one tenant-scoped transaction; joins a surrounding UoW when ctx
// carries one.
func (r *ReminderRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id reminder.ID,
	updateFn func(*reminder.Reminder) (bool, error),
) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *ReminderRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id reminder.ID,
	updateFn func(*reminder.Reminder) (bool, error),
) error {
	q := r.q.WithTx(tx)
	rem, err := loadReminder(ctx, q, id)
	if err != nil {
		return err
	}
	persist, err := updateFn(rem)
	if err != nil {
		return err
	}
	if !persist {
		_ = rem.PullEvents()
		return nil
	}
	if err := persistReminderState(ctx, q, rem); err != nil {
		return err
	}
	return drainReminderEvents(ctx, tx, rem)
}

// GetByID satisfies [reminder.Repository]. Tenant-scoped read.
func (r *ReminderRepository) GetByID(ctx context.Context, tenantID tenant.ID, id reminder.ID) (*reminder.Reminder, error) {
	var out *reminder.Reminder
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadReminder(ctx, q, id)
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

// ListPagePending satisfies [reminder.Repository]. Cursor-paginated per
// ADR 0038, sort tuple (due_at ASC, id ASC). GUC bound from explicit
// tenantID.
func (r *ReminderRepository) ListPagePending(
	ctx context.Context,
	tenantID tenant.ID,
	filter reminder.PendingFilter,
	cursor pagination.Cursor,
	pageSize int,
) (pagination.Page[*reminder.Reminder], error) {
	clamped := pagination.ClampPageSize(pageSize)
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*reminder.Reminder]{}, fmt.Errorf("crm reminder repo: parse tenant id %q: %w", tenantID, err)
	}
	params := db.ListPendingRemindersPageParams{
		TenantID:    pgconv.PgUUID(tid),
		Assignee:    uuidParamOpt(filter.AssigneeMembershipID),
		Type:        pgconv.ZeroToNil(filter.Type.String()),
		LeadID:      uuidParamOpt(filter.LeadID.String()),
		CursorDueAt: pgconv.PgTimestamp(cursor.SortValue),
		CursorID:    uuidParamOpt(cursor.ID),
		PageSize:    int32(clamped) + 1, //nolint:gosec // clamped ≤ 200 by ClampPageSize
	}
	var rows []db.CrmReminder
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := q.ListPendingRemindersPage(ctx, params)
		if err != nil {
			return fmt.Errorf("crm reminder repo: list page: %w", err)
		}
		rows = got
		return nil
	})
	if err != nil {
		return pagination.Page[*reminder.Reminder]{}, err
	}
	hasMore := false
	if len(rows) > clamped {
		hasMore = true
		rows = rows[:clamped]
	}
	items := make([]*reminder.Reminder, 0, len(rows))
	for _, row := range rows {
		hydrated, hErr := rowToReminder(row)
		if hErr != nil {
			return pagination.Page[*reminder.Reminder]{}, hErr
		}
		items = append(items, hydrated)
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = pagination.Encode(pagination.Cursor{SortValue: last.DueAt(), ID: last.ID().String()})
	}
	return pagination.Page[*reminder.Reminder]{
		Items:      items,
		HasMore:    hasMore,
		NextCursor: next,
	}, nil
}

// FindPendingMatureForLead satisfies [reminder.Repository]. Tenant-
// scoped hot-path probe for the mature-lead scheduler.
func (r *ReminderRepository) FindPendingMatureForLead(ctx context.Context, tenantID tenant.ID, leadID crmlead.ID) (*reminder.Reminder, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("crm reminder repo: parse tenant id %q: %w", tenantID, err)
	}
	lid, err := uuid.Parse(leadID.String())
	if err != nil {
		return nil, fmt.Errorf("crm reminder repo: parse lead id %q: %w", leadID, err)
	}
	var out *reminder.Reminder
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.FindPendingMatureForLead(ctx, db.FindPendingMatureForLeadParams{
			TenantID: pgconv.PgUUID(tid),
			LeadID:   pgconv.PgUUID(lid),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return reminder.ErrNotFound
			}
			return fmt.Errorf("crm reminder repo: find pending mature: %w", err)
		}
		hydrated, hErr := rowToReminder(row)
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

// ----- Helpers --------------------------------------------------------------

func loadReminder(ctx context.Context, q *db.Queries, id reminder.ID) (*reminder.Reminder, error) {
	rid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("crm reminder repo: parse id %q: %w", id, err)
	}
	row, err := q.GetReminderByID(ctx, pgconv.PgUUID(rid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, reminder.ErrNotFound
		}
		return nil, fmt.Errorf("crm reminder repo: get by id: %w", err)
	}
	return rowToReminder(row)
}

func insertReminderRow(ctx context.Context, q *db.Queries, r *reminder.Reminder) error {
	rid, err := uuid.Parse(r.ID().String())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse id %q: %w", r.ID(), err)
	}
	tid, err := uuid.Parse(r.TenantID().String())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse tenant id %q: %w", r.TenantID(), err)
	}
	lid, err := uuid.Parse(r.LeadID().String())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse lead id %q: %w", r.LeadID(), err)
	}
	assignedTo, err := uuid.Parse(r.AssignedToMembershipID())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse assigned_to membership id %q: %w", r.AssignedToMembershipID(), err)
	}
	if err := q.InsertReminder(ctx, db.InsertReminderParams{
		ID:                       pgconv.PgUUID(rid),
		TenantID:                 pgconv.PgUUID(tid),
		LeadID:                   pgconv.PgUUID(lid),
		AssignedToMembershipID:   pgconv.PgUUID(assignedTo),
		CreatedByMembershipID:    uuidParamOpt(r.CreatedByMembershipID()),
		SourceCallLogID:          uuidParamOpt(r.SourceCallLogID()),
		Type:                     r.Type().String(),
		State:                    r.State().String(),
		DueAt:                    pgconv.PgRequiredTimestamp(r.DueAt()),
		Notes:                    r.Notes(),
		SentAt:                   pgconv.PgTimestamp(r.SentAt()),
		MarkedSentByMembershipID: uuidParamOpt(r.MarkedSentByMembershipID()),
		CancelledAt:              pgconv.PgTimestamp(r.CancelledAt()),
		CancelledByMembershipID:  uuidParamOpt(r.CancelledByMembershipID()),
		CancelReason:             r.CancelReason(),
		CreatedAt:                pgconv.PgRequiredTimestamp(r.CreatedAt()),
	}); err != nil {
		// Translate the partial-unique-index 23505 fires (the
		// callback-pending + mature-pending guards) into the domain
		// sentinel so subscriber + scheduler treat duplicates as ACK
		// rather than as transient failures.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return reminder.ErrAlreadyExists
		}
		return fmt.Errorf("crm reminder repo: insert: %w", err)
	}
	return nil
}

func persistReminderState(ctx context.Context, q *db.Queries, r *reminder.Reminder) error {
	rid, err := uuid.Parse(r.ID().String())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse id %q: %w", r.ID(), err)
	}
	return q.UpdateReminderState(ctx, db.UpdateReminderStateParams{
		ID:                       pgconv.PgUUID(rid),
		State:                    r.State().String(),
		SentAt:                   pgconv.PgTimestamp(r.SentAt()),
		MarkedSentByMembershipID: uuidParamOpt(r.MarkedSentByMembershipID()),
		CancelledAt:              pgconv.PgTimestamp(r.CancelledAt()),
		CancelledByMembershipID:  uuidParamOpt(r.CancelledByMembershipID()),
		CancelReason:             r.CancelReason(),
	})
}

func drainReminderEvents(ctx context.Context, tx pgx.Tx, r *reminder.Reminder) error {
	evs := r.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(r.TenantID().String())
	if err != nil {
		return fmt.Errorf("crm reminder repo: parse tenant id %q: %w", r.TenantID(), err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("crm reminder repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func rowToReminder(row db.CrmReminder) (*reminder.Reminder, error) {
	rType, err := reminder.ParseType(row.Type)
	if err != nil {
		return nil, fmt.Errorf("crm reminder repo: stored type %q invalid: %w", row.Type, err)
	}
	rState, err := reminder.ParseState(row.State)
	if err != nil {
		return nil, fmt.Errorf("crm reminder repo: stored state %q invalid: %w", row.State, err)
	}
	return reminder.UnmarshalFromDB(reminder.Snapshot{
		ID:                       reminder.ID(pgconv.UUIDFromPg(row.ID).String()),
		TenantID:                 tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		LeadID:                   crmlead.ID(pgconv.UUIDFromPg(row.LeadID).String()),
		AssignedToMembershipID:   pgconv.UUIDFromPg(row.AssignedToMembershipID).String(),
		CreatedByMembershipID:    uuidStringOrEmpty(row.CreatedByMembershipID),
		SourceCallLogID:          uuidStringOrEmpty(row.SourceCallLogID),
		Type:                     rType,
		State:                    rState,
		DueAt:                    pgconv.TimeFromPg(row.DueAt),
		Notes:                    row.Notes,
		SentAt:                   pgconv.TimeFromPg(row.SentAt),
		MarkedSentByMembershipID: uuidStringOrEmpty(row.MarkedSentByMembershipID),
		CancelledAt:              pgconv.TimeFromPg(row.CancelledAt),
		CancelledByMembershipID:  uuidStringOrEmpty(row.CancelledByMembershipID),
		CancelReason:             row.CancelReason,
		CreatedAt:                pgconv.TimeFromPg(row.CreatedAt),
	}), nil
}
