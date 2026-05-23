package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// AssignmentHistoryRepository is the pgx/sqlc-backed implementation of
// [assignmenthistory.Repository]. Tenant-scoped.
type AssignmentHistoryRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewAssignmentHistoryRepository wires the repository.
func NewAssignmentHistoryRepository(pool *pgxpool.Pool, tx *pg.Transactor) *AssignmentHistoryRepository {
	return &AssignmentHistoryRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [assignmenthistory.Repository]. No events emitted from
// this aggregate per ADR 0060 — the parent CrmLead's AssignedEvent is
// the wire-side audit signal.
func (r *AssignmentHistoryRepository) Add(ctx context.Context, e *assignmenthistory.Entry) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, e)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, e)
	})
}

func (r *AssignmentHistoryRepository) addOnTx(ctx context.Context, tx pgx.Tx, e *assignmenthistory.Entry) error {
	q := r.q.WithTx(tx)
	id, err := uuid.Parse(e.ID().String())
	if err != nil {
		return fmt.Errorf("crm assignment_history repo: parse id %q: %w", e.ID(), err)
	}
	tid, err := uuid.Parse(e.TenantID())
	if err != nil {
		return fmt.Errorf("crm assignment_history repo: parse tenant id %q: %w", e.TenantID(), err)
	}
	lid, err := uuid.Parse(e.LeadID().String())
	if err != nil {
		return fmt.Errorf("crm assignment_history repo: parse lead id %q: %w", e.LeadID(), err)
	}
	assignee, err := uuid.Parse(e.AssigneeMembershipID())
	if err != nil {
		return fmt.Errorf("crm assignment_history repo: parse assignee %q: %w", e.AssigneeMembershipID(), err)
	}
	assignedBy, err := uuid.Parse(e.AssignedByMembershipID())
	if err != nil {
		return fmt.Errorf("crm assignment_history repo: parse assigned_by %q: %w", e.AssignedByMembershipID(), err)
	}
	if err := q.InsertAssignmentHistory(ctx, db.InsertAssignmentHistoryParams{
		ID:                           pgUUID(id),
		TenantID:                     pgUUID(tid),
		LeadID:                       pgUUID(lid),
		PreviousAssigneeMembershipID: uuidParamOpt(e.PreviousAssignee()),
		AssigneeMembershipID:         pgUUID(assignee),
		AssignedByMembershipID:       pgUUID(assignedBy),
		Reason:                       e.Reason(),
		AssignedAt:                   pgRequiredTimestamp(e.AssignedAt()),
		CreatedAt:                    pgRequiredTimestamp(e.CreatedAt()),
	}); err != nil {
		return fmt.Errorf("crm assignment_history repo: insert: %w", err)
	}
	return nil
}

// GetByID satisfies [assignmenthistory.Repository].
func (r *AssignmentHistoryRepository) GetByID(ctx context.Context, id assignmenthistory.ID) (*assignmenthistory.Entry, error) {
	hid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("crm assignment_history repo: parse id %q: %w", id, err)
	}
	var out *assignmenthistory.Entry
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetAssignmentHistoryByID(ctx, pgUUID(hid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assignmenthistory.ErrNotFound
			}
			return fmt.Errorf("crm assignment_history repo: get: %w", err)
		}
		out = rowToHistoryEntry(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByLead satisfies [assignmenthistory.Repository].
func (r *AssignmentHistoryRepository) ListByLead(ctx context.Context, leadID crmlead.ID) ([]*assignmenthistory.Entry, error) {
	lid, err := uuid.Parse(leadID.String())
	if err != nil {
		return nil, fmt.Errorf("crm assignment_history repo: parse lead id %q: %w", leadID, err)
	}
	var out []*assignmenthistory.Entry
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListAssignmentHistoryByLead(ctx, pgUUID(lid))
		if err != nil {
			return fmt.Errorf("crm assignment_history repo: list by lead: %w", err)
		}
		out = make([]*assignmenthistory.Entry, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowToHistoryEntry(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func rowToHistoryEntry(row db.CrmAssignmentHistory) *assignmenthistory.Entry {
	return assignmenthistory.UnmarshalFromDB(assignmenthistory.Snapshot{
		ID:                     assignmenthistory.ID(uuidFromPg(row.ID).String()),
		TenantID:               uuidFromPg(row.TenantID).String(),
		LeadID:                 crmlead.ID(uuidFromPg(row.LeadID).String()),
		PreviousAssignee:       uuidStringOrEmpty(row.PreviousAssigneeMembershipID),
		AssigneeMembershipID:   uuidFromPg(row.AssigneeMembershipID).String(),
		AssignedByMembershipID: uuidFromPg(row.AssignedByMembershipID).String(),
		Reason:                 row.Reason,
		AssignedAt:             timeFromPg(row.AssignedAt),
		CreatedAt:              timeFromPg(row.CreatedAt),
	})
}
