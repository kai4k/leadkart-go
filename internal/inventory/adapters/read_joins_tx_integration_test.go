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
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// TestProductRepository_GetByID_ReadsThroughTheSurroundingTx pins that the
// product read AddBatchHandler makes inside its UnitOfWork transaction uses that
// transaction. On a pool of ONE connection a read that asked for a connection of
// its own could never be served (the transaction holds the only one): the same
// shape that, at N concurrent add-batch calls on a pool of N, stalls every
// request of every tenant.
func TestProductRepository_GetByID_ReadsThroughTheSurroundingTx(t *testing.T) {
	t.Parallel()
	tid := seedTenant(t, repoFixture(t))

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
	products := adapters.NewProductRepository(pool, tx)
	ctx, cancel := context.WithTimeout(tenantCtx(t, tid), 5*time.Second)
	defer cancel()

	err = tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		_, gerr := products.GetByID(ctx, tid, product.ID(ids.NewV7().String()))
		return gerr
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("GetByID inside a tx on a one-connection pool = %v, want product.ErrNotFound at once (it must read through the tx, not wait for a second connection)", err)
	}
}
