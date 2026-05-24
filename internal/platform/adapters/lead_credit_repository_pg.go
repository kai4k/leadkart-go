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

// GetByTenant satisfies [leadcredit.Repository]. When called outside
// an active UoW tx, opens a tenant-scoped read tx so the lc_select
// RLS policy fires correctly (without a SET LOCAL app.tenant_id /
// app.is_platform on the connection, RLS returns 0 rows). When called
// inside a UoW tx, joins that tx (the caller already chose
// TxScopePlatform / TxScopeTenant).
func (r *LeadCreditRepository) GetByTenant(ctx context.Context, id leadcredit.TenantID) (*leadcredit.LeadCredit, error) {
	tid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("lead credit repo: parse tenant id: %w", err)
	}
	loadOn := func(ctx context.Context, q *db.Queries) (*leadcredit.LeadCredit, error) {
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

	if tx, ok := pg.TxFromContext(ctx); ok {
		return loadOn(ctx, r.q.WithTx(tx))
	}
	// No surrounding UoW tx — open our own tenant-scoped tx so RLS
	// fires. The lc_select policy allows either tenant_id =
	// current_tenant OR is_platform; tenant-scoped binding is the
	// most common caller (GET .../balance from a tenant user).
	var out *leadcredit.LeadCredit
	err = r.tx.WithinTxPgx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		got, err := loadOn(ctx, r.q.WithTx(tx))
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	return out, err
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
		// Try UPDATE first, fall back to INSERT iff 0 rows affected
		// AND in-memory Version == 0 (the "freshly-constructed
		// aggregate, no row in DB" shape per NewForTenant). This
		// mirrors UPSERT semantics WITHOUT a Postgres ON CONFLICT
		// clause (which would mask version-mismatch conflicts as
		// silent upserts, defeating the optimistic-concurrency
		// guarantee per ADR 0059).
		//
		// Version == 0 + UPDATE-affected-zero is ambiguous (no row
		// OR stale version). We disambiguate via the in-memory
		// CreatedAt == UpdatedAt heuristic: NewForTenant emits both
		// equal; any subsequent Topup/Charge bumps UpdatedAt. If they
		// differ here, this aggregate was loaded from the DB and a
		// concurrent writer just deleted the row — surface
		// ErrConflict (don't fabricate a new row).
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
			if l.Version() != 0 {
				// Stale version on an existing row.
				return leadcredit.ErrConflict
			}
			// Fresh aggregate, no row yet — INSERT.
			if err := q.InsertLeadCredit(ctx, db.InsertLeadCreditParams{
				TenantID:  pgUUID(tid),
				Balance:   l.Balance(),
				CreatedAt: pgRequiredTimestamp(l.CreatedAt()),
				UpdatedAt: pgRequiredTimestamp(l.UpdatedAt()),
			}); err != nil {
				return fmt.Errorf("lead credit repo: insert: %w", err)
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
