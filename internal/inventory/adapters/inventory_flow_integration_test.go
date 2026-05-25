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
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - Outbox row insertion in the SAME tx as the aggregate write
//     (ADR 0008); read-back via the messagingtest helper bypasses RLS.
//   - SQLSTATE 23505 translation on the per-tenant partial unique
//     index `uq_products_tenant_sku_live` → typed [product.ErrSKUTaken].
//
// Business-rule + state-machine + round-trip coverage moved to the
// per-aggregate fakes:
//   - [batchtest.FakeRepository] / [producttest.FakeRepository] /
//     [stockmovementtest.FakeRepository].

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// TestProductRepository_Add_DrainsCreatedEventToOutbox — SQL-contract:
// every product.Add MUST also write a row to inventory.outbox in the
// same tx per ADR 0008. The read-back uses the messagingtest helper
// (RLS+FORCE on outbox is bypassed inside the helper via the platform
// GUC); confirms the topic + tenant_id round-trip.
func TestProductRepository_Add_DrainsCreatedEventToOutbox(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, err := product.New(
		product.ID(ids.NewV7().String()),
		tid, actor,
		product.Spec{
			SKU: "AMOX-500", Name: "Amoxicillin 500 mg",
			DosageForm: "Capsule", PackSize: "10x10",
			HSNCode: "30049099", GSTRateBps: 1200,
			Manufacturer: "Acme",
		},
		fixedNow,
	)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tidUUID, err := uuid.Parse(tid.String())
	if err != nil {
		t.Fatalf("uuid.Parse(tid): %v", err)
	}
	count := messagingtest.OutboxCountByTenant(t, pool, messagingtest.SchemaInventory, tidUUID)
	if count < 1 {
		t.Fatalf("outbox: got %d rows want >= 1 (created event)", count)
	}
}

// TestProductRepository_Add_DuplicateSKU_ReturnsErrSKUTaken — SQL-contract:
// SQLSTATE 23505 on the per-tenant partial unique index
// `uq_products_tenant_sku_live` MUST surface as the typed domain
// sentinel [product.ErrSKUTaken].
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
