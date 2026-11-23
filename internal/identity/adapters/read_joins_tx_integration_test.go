//go:build integration

// arch-test:parallel-safe — own one-connection pool on the shared pgtest
// container, fresh tenant_id, RLS isolation.

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TestRoleHierarchyEdgeRepository_GetActiveByChild_ReadsThroughTheSurroundingTx
// pins that the edge read SetRoleParentHandler makes under the tenant authority
// lock uses that transaction. On a pool of ONE connection a read that asked for a
// connection of its own could never be served (the transaction holds the only
// one): the shape that, at N concurrent calls on a pool of N, stalls every
// request of every tenant.
func TestRoleHierarchyEdgeRepository_GetActiveByChild_ReadsThroughTheSurroundingTx(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig(sharedPG.AppDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	tx := pg.NewTransactor(pool)
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, tx)
	tid := tenant.ID(ids.NewV7().String())
	ctx, cancel := context.WithTimeout(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), 5*time.Second)
	defer cancel()

	err = tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		_, gerr := edges.GetActiveByChild(ctx, tid, role.ID(ids.NewV7().String()))
		return gerr
	})
	if !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Fatalf("GetActiveByChild inside a tx on a one-connection pool = %v, want ErrEdgeNotFound at once (it must read through the tx, not wait for a second connection)", err)
	}
}
