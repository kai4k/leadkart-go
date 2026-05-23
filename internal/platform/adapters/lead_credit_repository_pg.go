package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// LeadCreditRepository is the pgx/sqlc-backed implementation of
// [leadcredit.Repository]. Optimistic concurrency via the explicit
// `version` column per ADR 0059.
type LeadCreditRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewLeadCreditRepository wires the repository.
func NewLeadCreditRepository(pool *pgxpool.Pool, tx *pg.Transactor) *LeadCreditRepository {
	return &LeadCreditRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// GetByTenant satisfies [leadcredit.Repository].
func (r *LeadCreditRepository) GetByTenant(ctx context.Context, id leadcredit.TenantID) (*leadcredit.LeadCredit, error) {
	tid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("lead credit repo: parse tenant id: %w", err)
	}
	q := r.q
	if tx, ok := pg.TxFromContext(ctx); ok {
		q = r.q.WithTx(tx)
	}
	row, err := q.GetLeadCredit(ctx, pgUUID(tid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, leadcredit.ErrNotFound
		}
		return nil, fmt.Errorf("lead credit repo: get: %w", err)
	}
	return leadcredit.UnmarshalFromDB(leadcredit.Snapshot{
		TenantID:  leadcredit.TenantID(uuidFromPg(row.TenantID).String()),
		Balance:   row.Balance,
		Version:   row.Version,
		CreatedAt: timeFromPg(row.CreatedAt),
		UpdatedAt: timeFromPg(row.UpdatedAt),
	}), nil
}

// UpsertWithVersion satisfies [leadcredit.Repository]. Aggregate's
// in-memory Version is the expected pre-update value. On INSERT path
// (Version == 0 + no row exists), we INSERT. On UPDATE path, we run
// `UPDATE ... WHERE version = expected_version`; 0 rows = ErrConflict.
//
// Drains the aggregate's events to the outbox AFTER the persist
// succeeds (so a conflict aborts cleanly).
func (r *LeadCreditRepository) UpsertWithVersion(ctx context.Context, l *leadcredit.LeadCredit) error {
	run := func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		tid, err := uuid.Parse(l.TenantID().String())
		if err != nil {
			return fmt.Errorf("lead credit repo: parse tenant id: %w", err)
		}
		// Version == 0 + no row → INSERT path. We detect INSERT by
		// running the UPDATE first; if 0 rows + Version == 0, try
		// INSERT (the common case: first topup on a fresh tenant).
		if l.Version() == 0 {
			err := q.InsertLeadCredit(ctx, db.InsertLeadCreditParams{
				TenantID:  pgUUID(tid),
				Balance:   l.Balance(),
				CreatedAt: pgRequiredTimestamp(l.CreatedAt()),
				UpdatedAt: pgRequiredTimestamp(l.UpdatedAt()),
			})
			if err != nil {
				return fmt.Errorf("lead credit repo: insert: %w", err)
			}
		} else {
			affected, err := q.UpdateLeadCreditWithVersion(ctx, db.UpdateLeadCreditWithVersionParams{
				NewBalance:      l.Balance(),
				ExpectedVersion: l.Version(),
				UpdatedAt:       pgRequiredTimestamp(l.UpdatedAt()),
				TenantID:        pgUUID(tid),
			})
			if err != nil {
				return fmt.Errorf("lead credit repo: update: %w", err)
			}
			if affected == 0 {
				return leadcredit.ErrConflict
			}
		}

		// Drain LeadCredit's AdjustedEvent → LeadCreditAdjustedV1 via
		// the mechanical mapper. tenant_id on the outbox row is the
		// LeadCredit's tenant (the aggregate IS tenant-scoped).
		evs := l.PullEvents()
		if len(evs) == 0 {
			return nil
		}
		asAny := make([]any, len(evs))
		for i, e := range evs {
			asAny[i] = e
		}
		return drainEventsToOutbox(ctx, tx, tid, asAny)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, run)
}
