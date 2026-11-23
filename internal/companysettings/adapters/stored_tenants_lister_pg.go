package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters/db"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/jobs"
)

// StoredTenantsLister is the pgx/sqlc-backed [jobs.TenantLister]. The republish
// sweep is the one cross-tenant read of this module, so it runs under platform
// scope (TxScopePlatform); the per-tenant republish that follows runs under the
// tenant's own scope.
type StoredTenantsLister struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewStoredTenantsLister wires the lister.
func NewStoredTenantsLister(pool *pgxpool.Pool, tx *pg.Transactor) *StoredTenantsLister {
	return &StoredTenantsLister{pool: pool, tx: tx, q: db.New(pool)}
}

var _ jobs.TenantLister = (*StoredTenantsLister)(nil)

// ListStoredTenants returns up to limit tenants that have a stored policy
// (version 1 or later), ordered by id, after the given id.
func (l *StoredTenantsLister) ListStoredTenants(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	var rows []uuid.UUID
	err := l.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		got, qerr := l.q.WithTx(tx).ListStoredPolicyTenants(ctx, db.ListStoredPolicyTenantsParams{
			TenantID: pgconv.PgUUID(after), Limit: int32(limit), //nolint:gosec // limit is jobs.PageSize
		})
		for _, id := range got {
			rows = append(rows, pgconv.UUIDFromPg(id))
		}
		return qerr
	})
	if err != nil {
		return nil, fmt.Errorf("stored tenants lister: %w", err)
	}
	return rows, nil
}
