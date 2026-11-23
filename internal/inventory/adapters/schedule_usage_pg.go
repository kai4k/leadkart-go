package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
)

// ScheduleUsageReader counts the live products that carry a drug schedule class
// (TxScopeTenant semantics: the GUC is bound from the explicit tenant id).
type ScheduleUsageReader struct {
	tx *pg.Transactor
	q  *db.Queries
}

var _ query.ScheduleUsageReader = (*ScheduleUsageReader)(nil)

// NewScheduleUsageReader constructs the reader.
func NewScheduleUsageReader(pool *pgxpool.Pool, tx *pg.Transactor) *ScheduleUsageReader {
	return &ScheduleUsageReader{tx: tx, q: db.New(pool)}
}

// CountWithAnyClass satisfies [query.ScheduleUsageReader].
func (r *ScheduleUsageReader) CountWithAnyClass(ctx context.Context, tenantID tenant.ID) (int64, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return 0, fmt.Errorf("schedule usage: tenant id: %w", err)
	}
	var n int64
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		var qerr error
		n, qerr = r.q.WithTx(tx).CountProductsWithScheduleClass(ctx, pgconv.PgUUID(tid))
		return qerr
	})
	return n, err
}

// CountWithClass satisfies [query.ScheduleUsageReader].
func (r *ScheduleUsageReader) CountWithClass(ctx context.Context, tenantID tenant.ID, classID string) (int64, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return 0, fmt.Errorf("schedule usage: tenant id: %w", err)
	}
	cid, err := uuid.Parse(classID)
	if err != nil {
		return 0, fmt.Errorf("schedule usage: class id: %w", err)
	}
	var n int64
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		var qerr error
		n, qerr = r.q.WithTx(tx).CountProductsWithClass(ctx, db.CountProductsWithClassParams{TenantID: pgconv.PgUUID(tid), DrugScheduleClassID: pgconv.PgUUID(cid)})
		return qerr
	})
	return n, err
}
