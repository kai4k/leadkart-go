package adapters

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PermissionHolderReader lists the active memberships of a company that hold a
// permission through a role (TxScopeTenant semantics: the GUC is bound from the
// explicit tenant id). Other modules reach it only through a port bound at
// the composition root.
type PermissionHolderReader struct {
	tx *pg.Transactor
	q  *db.Queries
}

// NewPermissionHolderReader constructs the reader.
func NewPermissionHolderReader(pool *pgxpool.Pool, tx *pg.Transactor) *PermissionHolderReader {
	return &PermissionHolderReader{tx: tx, q: db.New(pool)}
}

// ActiveMembershipsHoldingAny returns the active memberships of the tenant whose
// roles grant at least one of permissions (or are super-admin roles).
func (r *PermissionHolderReader) ActiveMembershipsHoldingAny(ctx context.Context, tenantID tenant.ID, permissions []string) ([]membership.ID, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("permission holders: tenant id: %w", err)
	}
	var out []membership.ID
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := r.q.WithTx(tx).ListActiveMembershipsHoldingAnyPermission(ctx, db.ListActiveMembershipsHoldingAnyPermissionParams{
			TenantID: pgconv.PgUUID(tid), Permissions: permissions,
		})
		if qerr != nil {
			return qerr
		}
		for _, id := range rows {
			out = append(out, membership.ID(pgconv.UUIDFromPg(id).String()))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("permission holders: %w", err)
	}
	return out, nil
}
