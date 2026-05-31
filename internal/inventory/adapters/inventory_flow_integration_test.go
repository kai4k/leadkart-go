//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each other's state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.
//
// SQL-contract coverage (ADR 0062): SQLSTATE 23505 on
// uq_products_tenant_sku_live → [product.ErrSKUTaken].
//
// Outbox-emission coverage lives in outbox_forwarder_integration_test.go;
// business-rule + round-trip coverage in per-aggregate fakes.

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// TestProductRepository_Add_DuplicateSKU_ReturnsErrSKUTaken verifies that
// SQLSTATE 23505 on uq_products_tenant_sku_live surfaces as [product.ErrSKUTaken].
func TestProductRepository_Add_DuplicateSKU_ReturnsErrSKUTaken(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	spec := product.Spec{
		SKU: "DUP-1", Name: "First",
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: 1200,
	}
	first, _ := product.New(product.ID(ids.NewV7().String()), tid, actor, spec, fixedNow)
	if err := products.Add(ctx, first); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	second, _ := product.New(product.ID(ids.NewV7().String()), tid, actor, spec, fixedNow)
	err := products.Add(ctx, second)
	if !errors.Is(err, product.ErrSKUTaken) {
		t.Fatalf("want ErrSKUTaken, got %v", err)
	}
}
