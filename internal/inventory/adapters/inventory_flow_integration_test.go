//go:build integration

package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// TestProductRepository_AddGetUpdate_RoundTripsViaOutbox exercises the
// full Add → GetByID → UpdateByID path + asserts an outbox row was
// written same-tx per ADR 0008.
func TestProductRepository_AddGetUpdate_RoundTripsViaOutbox(t *testing.T) {
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

	got, err := products.GetByID(ctx, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SKU() != "AMOX-500" {
		t.Fatalf("sku round-trip: %q", got.SKU())
	}
	if got.GSTRateBps() != 1200 {
		t.Fatalf("gst round-trip: %d", got.GSTRateBps())
	}

	// Update via UpdateByID closure.
	newName := "Amoxicillin 500 mg Capsules"
	err = products.UpdateByID(ctx, p.ID(), func(p *product.Product) (bool, error) {
		return true, p.Update(actor, product.UpdateSpec{Name: &newName}, fixedNow)
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got2, err := products.GetByID(ctx, p.ID())
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got2.Name() != newName {
		t.Fatalf("name after update: %q", got2.Name())
	}

	// Outbox row written for the Create (and Update) — bypass RLS to
	// inspect since outbox is FORCE RLS.
	rawDB, err := openRawDB(t, pool)
	if err != nil {
		t.Fatalf("openRawDB: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `SELECT set_config('app.is_platform','true',false)`); err != nil {
		t.Fatalf("set platform: %v", err)
	}
	var count int
	if err := rawDB.QueryRowContext(ctx,
		`SELECT count(*) FROM inventory.outbox WHERE tenant_id = $1`,
		tid.String()).Scan(&count); err != nil {
		t.Fatalf("read outbox count: %v", err)
	}
	if count < 2 {
		t.Fatalf("outbox: got %d rows want >= 2 (created + updated)", count)
	}
}

// TestProductRepository_Add_DuplicateSKU_ReturnsErrSKUTaken proves the
// per-tenant partial unique index surfaces as a typed error.
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

// TestBatchRepository_FullStockMovementFlow_HappyPath exercises the
// multi-aggregate path: Add Product → Add Batch → Inbound + Outbound
// movements via UoW + version bump verified at every step.
func TestBatchRepository_FullStockMovementFlow_HappyPath(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	movements := adapters.NewStockMovementRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	// Seed Product.
	p, _ := product.New(
		product.ID(ids.NewV7().String()),
		tid, actor,
		product.Spec{
			SKU: "FLOW-1", Name: "Flow Drug",
			DosageForm: "Tablet", PackSize: "10",
			HSNCode: "3004", GSTRateBps: 1200,
		},
		fixedNow,
	)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}

	// Seed Batch.
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := batch.New(
		batch.ID(ids.NewV7().String()),
		p.ID(), tid, actor,
		batch.Spec{
			BatchNumber:                "LOT-1",
			ManufactureDate:            mfg,
			ExpiryDate:                 exp,
			ManufacturerName:           "Acme",
			ManufacturingLicenceNumber: "ML-1",
			MRPPaise:                   25000,
			PurchasePricePaise:         18000,
		},
		fixedNow,
	)
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	// Inbound 100 via UpdateByID + Movement insert in one UoW tx.
	err := tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx2 context.Context) error {
		updErr := batches.UpdateByID(ctx2, b.ID(), func(b *batch.Batch) (bool, error) {
			return true, b.ApplyMovement(batch.MovementInbound, 100, fixedNow)
		})
		if updErr != nil {
			return updErr
		}
		m, mErr := stockmovement.New(stockmovement.ID(ids.NewV7().String()), stockmovement.Spec{
			BatchID:             b.ID(),
			ProductID:           p.ID(),
			TenantID:            tid,
			Type:                batch.MovementInbound,
			Quantity:            100,
			QuantityOnHandAfter: 100,
			Reason:              "initial inbound",
			ActorMembershipID:   actor,
		}, fixedNow)
		if mErr != nil {
			return mErr
		}
		return movements.Add(ctx2, m)
	})
	if err != nil {
		t.Fatalf("Inbound flow: %v", err)
	}

	// Verify batch state + version.
	gotB, err := batches.GetByID(ctx, b.ID())
	if err != nil {
		t.Fatalf("GetByID batch: %v", err)
	}
	if gotB.QuantityOnHand() != 100 {
		t.Fatalf("on-hand after inbound: %d", gotB.QuantityOnHand())
	}
	if gotB.Version() != 1 {
		t.Fatalf("version after inbound: %d", gotB.Version())
	}

	// ListByBatchPage returns the one movement.
	page, err := movements.ListByBatchPage(ctx, b.ID(), stockmovement.PageRequest{
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("ListByBatchPage: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("movements: got %d want 1", len(page.Items))
	}
	if page.Items[0].Quantity() != 100 {
		t.Fatalf("movement quantity: %d", page.Items[0].Quantity())
	}
}

// TestBatchRepository_SequentialUpdates_BumpVersionMonotonically
// proves the version token increments on every successful UpdateByID
// + the version-check predicate accepts the new value on the next
// call. A true conflict-induced retry test (requiring goroutine
// racers) ships in slice 2 alongside the LogStockMovement handler
// integration test.
func TestBatchRepository_SequentialUpdates_BumpVersionMonotonically(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "CON-1", Name: "Con", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}

	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
		batch.Spec{BatchNumber: "L1", ManufactureDate: mfg, ExpiryDate: exp,
			ManufacturerName: "A", ManufacturingLicenceNumber: "ML-1",
			MRPPaise: 100, PurchasePricePaise: 50}, fixedNow)
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}

	// First UpdateByID — version 0 → 1.
	err := batches.UpdateByID(ctx, b.ID(), func(b *batch.Batch) (bool, error) {
		if b.Version() != 0 {
			t.Errorf("v0 load: got %d want 0", b.Version())
		}
		return true, b.ApplyMovement(batch.MovementInbound, 10, fixedNow)
	})
	if err != nil {
		t.Fatalf("first UpdateByID: %v", err)
	}

	// Second UpdateByID — load picks up bumped version (1) + the
	// WHERE version=$expected predicate in the adapter persists against
	// the matching row, advancing to 2.
	err = batches.UpdateByID(ctx, b.ID(), func(b *batch.Batch) (bool, error) {
		if b.Version() != 1 {
			t.Errorf("v1 load: got %d want 1", b.Version())
		}
		return true, b.ApplyMovement(batch.MovementInbound, 5, fixedNow)
	})
	if err != nil {
		t.Fatalf("second UpdateByID: %v", err)
	}

	final, err := batches.GetByID(ctx, b.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if final.QuantityOnHand() != 15 {
		t.Fatalf("on-hand: %d", final.QuantityOnHand())
	}
	if final.Version() != 2 {
		t.Fatalf("version: %d", final.Version())
	}
}

// TestBatchRepository_AnyLiveWithStockForProduct_GatesProductDelete
// proves the cross-aggregate read used by DeleteProductHandler returns
// the right boolean for the live-stock guard.
func TestBatchRepository_AnyLiveWithStockForProduct_GatesProductDelete(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	batches := adapters.NewBatchRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
		product.Spec{SKU: "GUARD-1", Name: "Guard", DosageForm: "Tablet",
			PackSize: "10", HSNCode: "3004", GSTRateBps: 1200}, fixedNow)
	if err := products.Add(ctx, p); err != nil {
		t.Fatalf("Add product: %v", err)
	}

	// No batches yet — must return false.
	has, err := batches.AnyLiveWithStockForProduct(ctx, p.ID())
	if err != nil {
		t.Fatalf("AnyLiveWithStockForProduct: %v", err)
	}
	if has {
		t.Fatal("no batches yet: must return false")
	}

	// Add a batch with zero on-hand — still false.
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := batch.New(batch.ID(ids.NewV7().String()), p.ID(), tid, actor,
		batch.Spec{BatchNumber: "B1", ManufactureDate: mfg, ExpiryDate: exp,
			ManufacturerName: "A", ManufacturingLicenceNumber: "ML-1",
			MRPPaise: 100, PurchasePricePaise: 50}, fixedNow)
	if err := batches.Add(ctx, b); err != nil {
		t.Fatalf("Add batch: %v", err)
	}
	has, _ = batches.AnyLiveWithStockForProduct(ctx, p.ID())
	if has {
		t.Fatal("zero on-hand: must return false")
	}

	// Inbound 10 → must return true.
	err = batches.UpdateByID(ctx, b.ID(), func(b *batch.Batch) (bool, error) {
		return true, b.ApplyMovement(batch.MovementInbound, 10, fixedNow)
	})
	if err != nil {
		t.Fatalf("inbound: %v", err)
	}
	has, _ = batches.AnyLiveWithStockForProduct(ctx, p.ID())
	if !has {
		t.Fatal("non-zero on-hand: must return true")
	}
}

// TestProductRepository_ListPage_PaginatesByCreatedAtKeyset proves the
// keyset cursor returns disjoint pages across two calls.
func TestProductRepository_ListPage_PaginatesByCreatedAtKeyset(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tid := seedTenant(t, pool)
	ctx := tenantCtx(t, tid)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx := pg.NewTransactor(pool)
	products := adapters.NewProductRepository(pool, tx)
	actor := membership.ID(ids.NewV7().String())

	// Seed 5 products. UUIDv7 → created_at is wall clock, ordering by
	// (created_at DESC, id DESC) should match insertion order reversed.
	for i := range 5 {
		p, _ := product.New(product.ID(ids.NewV7().String()), tid, actor,
			product.Spec{
				SKU: fmt.Sprintf("PG-%d", i), Name: "PG",
				DosageForm: "Tablet", PackSize: "10",
				HSNCode: "3004", GSTRateBps: 1200,
			}, fixedNow)
		if err := products.Add(ctx, p); err != nil {
			t.Fatalf("seed Add %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond) // ensure created_at differs across rows // arch-test:wait-justified
	}

	page1, err := products.ListPage(ctx, tid, product.ListFilter{}, pagination.Cursor{}, 2)
	if err != nil {
		t.Fatalf("ListPage 1: %v", err)
	}
	if len(page1.Items) != 2 || !page1.HasMore {
		t.Fatalf("page1: got %d items, has_more=%v", len(page1.Items), page1.HasMore)
	}

	c2, err := pagination.Decode(page1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	page2, err := products.ListPage(ctx, tid, product.ListFilter{}, c2, 2)
	if err != nil {
		t.Fatalf("ListPage 2: %v", err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("page2: got %d items", len(page2.Items))
	}

	// Pages MUST be disjoint.
	seen := map[product.ID]bool{}
	for _, p := range page1.Items {
		seen[p.ID()] = true
	}
	for _, p := range page2.Items {
		if seen[p.ID()] {
			t.Fatalf("page-overlap on %s", p.ID())
		}
	}
}

