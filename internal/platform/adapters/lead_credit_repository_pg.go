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
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// LeadCreditRepository is the pgx/sqlc implementation of
// [leadcredit.Repository]. Optimistic concurrency via the explicit version
// column (ADR 0059).
//
// Tenancy (ADR 0062): the GUC binds from an EXPLICIT tenantID (the
// GetByTenant param or l.TenantID()), never from ctx — so this repo opens its
// own tenant tx via WithinTxPgxTenant, not the TxScopeTenant scope enum
// (TestArch_RepoTenantScopedReadsUseTxScopeTenant keys its exemption on this
// note). The lc_* RLS policies (tenant_id = current_tenant() OR is_platform())
// gate per-tenant access. Cross-tenant work (forwarder, support tooling) uses
// a platform-scoped transactor higher up.
type LeadCreditRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewLeadCreditRepository wires the repository.
//
//nolint:revive // factory shape mirrors siblings
func NewLeadCreditRepository(pool *pgxpool.Pool, tx *pg.Transactor) *LeadCreditRepository {
	return &LeadCreditRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// GetByTenant satisfies [leadcredit.Repository]. Tenant-scoped read; GUC
// binds from the explicit tenantID (ADR 0062). Joins any UoW tx in ctx.
func (r *LeadCreditRepository) GetByTenant(ctx context.Context, id leadcredit.TenantID) (*leadcredit.LeadCredit, error) {
	tid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("lead credit repo: parse tenant id: %w", err)
	}
	loadOn := func(ctx context.Context, q *db.Queries) (*leadcredit.LeadCredit, error) {
		row, err := q.GetLeadCredit(ctx, pgconv.PgUUID(tid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, leadcredit.ErrNotFound
			}
			return nil, fmt.Errorf("lead credit repo: get: %w", err)
		}
		return leadcredit.UnmarshalFromDB(leadcredit.Snapshot{
			TenantID:  leadcredit.TenantID(pgconv.UUIDFromPg(row.TenantID).String()),
			Balance:   row.Balance,
			Version:   row.Version,
			CreatedAt: pgconv.TimeFromPg(row.CreatedAt),
			UpdatedAt: pgconv.TimeFromPg(row.UpdatedAt),
		}), nil
	}

	if tx, ok := pg.TxFromContext(ctx); ok {
		return loadOn(ctx, r.q.WithTx(tx))
	}
	// No UoW tx: open our own tenant-scoped tx from the explicit tenantID;
	// lc_select admits via tenant_id = current_tenant().
	var out *leadcredit.LeadCredit
	err = r.tx.WithinTxPgxTenant(ctx, id.String(), func(ctx context.Context, tx pgx.Tx) error {
		got, err := loadOn(ctx, r.q.WithTx(tx))
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	return out, err
}

// UpsertWithVersion satisfies [leadcredit.Repository]. The aggregate's
// in-memory Version is the expected pre-update value: UPDATE ... WHERE
// version = expected; 0 rows affected means INSERT (fresh) or ErrConflict
// (stale). Events drain to the outbox only after the persist succeeds, so a
// conflict aborts cleanly. GUC binds from l.TenantID() when outside a UoW tx
// (ADR 0062).
func (r *LeadCreditRepository) UpsertWithVersion(ctx context.Context, l *leadcredit.LeadCredit) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.upsertOnTx(ctx, tx, l)
	}
	return r.tx.WithinTxPgxTenant(ctx, l.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.upsertOnTx(ctx, tx, l)
	})
}

func (r *LeadCreditRepository) upsertOnTx(ctx context.Context, tx pgx.Tx, l *leadcredit.LeadCredit) error {
	q := r.q.WithTx(tx)
	tid, err := uuid.Parse(l.TenantID().String())
	if err != nil {
		return fmt.Errorf("lead credit repo: parse tenant id: %w", err)
	}
	// UPDATE first; fall back to INSERT only when 0 rows affected AND
	// Version == 0 (fresh aggregate, no row yet). No ON CONFLICT: it would
	// mask version mismatches as silent upserts, defeating optimistic
	// concurrency (ADR 0059).
	affected, err := q.UpdateLeadCreditWithVersion(ctx, db.UpdateLeadCreditWithVersionParams{
		NewBalance:      l.Balance(),
		ExpectedVersion: l.Version(),
		UpdatedAt:       pgconv.PgRequiredTimestamp(l.UpdatedAt()),
		TenantID:        pgconv.PgUUID(tid),
	})
	if err != nil {
		return fmt.Errorf("lead credit repo: update: %w", err)
	}
	if affected == 0 {
		if l.Version() != 0 {
			// Stale version on an existing row.
			return leadcredit.ErrConflict
		}
		if err := q.InsertLeadCredit(ctx, db.InsertLeadCreditParams{
			TenantID:  pgconv.PgUUID(tid),
			Balance:   l.Balance(),
			CreatedAt: pgconv.PgRequiredTimestamp(l.CreatedAt()),
			UpdatedAt: pgconv.PgRequiredTimestamp(l.UpdatedAt()),
		}); err != nil {
			return fmt.Errorf("lead credit repo: insert: %w", err)
		}
	}

	// Drain AdjustedEvent to the outbox stamped with the aggregate's tenant.
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
