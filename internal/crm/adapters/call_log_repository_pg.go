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
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CallLogRepository is the pgx/sqlc-backed implementation of
// [calllog.Repository]. Tenant-scoped — every read + write runs under
// [pg.TxScopeTenant]. GUC bound from explicit tenantID per ADR 0062
// (TDL canon).
type CallLogRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewCallLogRepository wires the repository.
func NewCallLogRepository(pool *pgxpool.Pool, tx *pg.Transactor) *CallLogRepository {
	return &CallLogRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [calllog.Repository]. Drains events into the outbox in
// the same tx; joins a surrounding UoW when ctx carries one. The
// aggregate carries its own TenantID — the GUC is bound from
// c.TenantID() (TDL canon per ADR 0062).
func (r *CallLogRepository) Add(ctx context.Context, c *calllog.CallLog) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, c)
	}
	return r.tx.WithinTxPgxTenant(ctx, c.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, c)
	})
}

func (r *CallLogRepository) addOnTx(ctx context.Context, tx pgx.Tx, c *calllog.CallLog) error {
	q := r.q.WithTx(tx)
	cid, err := uuid.Parse(c.ID().String())
	if err != nil {
		return fmt.Errorf("crm call_log repo: parse id %q: %w", c.ID(), err)
	}
	tid, err := uuid.Parse(c.TenantID().String())
	if err != nil {
		return fmt.Errorf("crm call_log repo: parse tenant id %q: %w", c.TenantID(), err)
	}
	lid, err := uuid.Parse(c.LeadID().String())
	if err != nil {
		return fmt.Errorf("crm call_log repo: parse lead id %q: %w", c.LeadID(), err)
	}
	loggedByID, err := uuid.Parse(c.LoggedByMembershipID())
	if err != nil {
		return fmt.Errorf("crm call_log repo: parse logged_by membership id %q: %w", c.LoggedByMembershipID(), err)
	}
	if err := q.InsertCallLog(ctx, db.InsertCallLogParams{
		ID:                   pgUUID(cid),
		TenantID:             pgUUID(tid),
		LeadID:               pgUUID(lid),
		Outcome:              c.Outcome().String(),
		Notes:                c.Notes(),
		LoggedByMembershipID: pgUUID(loggedByID),
		LoggedAt:             pgRequiredTimestamp(c.LoggedAt()),
		CreatedAt:            pgRequiredTimestamp(c.CreatedAt()),
	}); err != nil {
		return fmt.Errorf("crm call_log repo: insert: %w", err)
	}
	return drainCallLogEvents(ctx, tx, c, tid)
}

// GetByID satisfies [calllog.Repository]. Tenant-scoped read — GUC
// bound from the explicit tenantID parameter (TDL canon per ADR 0062).
func (r *CallLogRepository) GetByID(ctx context.Context, tenantID tenant.ID, id calllog.ID) (*calllog.CallLog, error) {
	cid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("crm call_log repo: parse id %q: %w", id, err)
	}
	var out *calllog.CallLog
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetCallLogByID(ctx, pgUUID(cid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return calllog.ErrNotFound
			}
			return fmt.Errorf("crm call_log repo: get: %w", err)
		}
		out = rowToCallLog(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByLead satisfies [calllog.Repository]. Tenant-scoped read — GUC
// bound from the explicit tenantID parameter.
func (r *CallLogRepository) ListByLead(ctx context.Context, tenantID tenant.ID, leadID crmlead.ID) ([]*calllog.CallLog, error) {
	lid, err := uuid.Parse(leadID.String())
	if err != nil {
		return nil, fmt.Errorf("crm call_log repo: parse lead id %q: %w", leadID, err)
	}
	var out []*calllog.CallLog
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListCallLogsByLead(ctx, pgUUID(lid))
		if err != nil {
			return fmt.Errorf("crm call_log repo: list by lead: %w", err)
		}
		out = make([]*calllog.CallLog, 0, len(rows))
		for _, row := range rows {
			out = append(out, rowToCallLog(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func drainCallLogEvents(ctx context.Context, tx pgx.Tx, c *calllog.CallLog, tenant uuid.UUID) error {
	evs := c.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("crm call_log repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tenant, mapped)
}

func rowToCallLog(row db.CrmCallLog) *calllog.CallLog {
	outcome := calllog.Outcome(row.Outcome)
	return calllog.UnmarshalFromDB(calllog.Snapshot{
		ID:                   calllog.ID(uuidFromPg(row.ID).String()),
		TenantID:             tenant.ID(uuidFromPg(row.TenantID).String()),
		LeadID:               crmlead.ID(uuidFromPg(row.LeadID).String()),
		Outcome:              outcome,
		Notes:                row.Notes,
		LoggedByMembershipID: uuidFromPg(row.LoggedByMembershipID).String(),
		LoggedAt:             timeFromPg(row.LoggedAt),
		CreatedAt:            timeFromPg(row.CreatedAt),
	})
}
