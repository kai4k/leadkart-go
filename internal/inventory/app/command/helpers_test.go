package command_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch/batchtest"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product/producttest"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement/stockmovementtest"
)

// Test-local aggregate-ID factories mirroring the production wiring shape.
func testNewProductID() product.ID        { return product.ID(ids.NewV7().String()) }
func testNewBatchID() batch.ID            { return batch.ID(ids.NewV7().String()) }
func testNewMovementID() stockmovement.ID { return stockmovement.ID(ids.NewV7().String()) }

// fixedNow is the deterministic instant tests pass to every factory and mutator.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func newMembershipID(t *testing.T) membership.ID {
	t.Helper()
	return membership.ID(ids.NewV7().String())
}

// seedProduct creates and Adds a Product; SKU is per-call so tests can drive
// ErrSKUTaken paths.
func seedProduct(t *testing.T, repo *producttest.FakeRepository, tid tenant.ID, actor membership.ID, sku string) *product.Product {
	t.Helper()
	p, err := product.New(
		product.ID(ids.NewV7().String()),
		tid, actor,
		product.Spec{
			SKU: sku, Name: "Test Drug",
			DosageForm: "Tablet", PackSize: "10",
			HSNCode: "3004", GSTRateBps: 1200,
		},
		fixedNow,
	)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return p
}

// seedBatch creates and Adds a live Batch for the product, starting at
// quantity_on_hand=0.
func seedBatch(t *testing.T, repo *batchtest.FakeRepository, p *product.Product, actor membership.ID, batchNumber string) *batch.Batch {
	t.Helper()
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC)
	b, err := batch.New(
		batch.ID(ids.NewV7().String()),
		p.ID(), p.TenantID(), actor,
		batch.Spec{
			BatchNumber:                batchNumber,
			ManufactureDate:            mfg,
			ExpiryDate:                 exp,
			ManufacturerName:           "Acme",
			ManufacturingLicenceNumber: "ML-1",
			MRPPaise:                   25000,
			PurchasePricePaise:         18000,
		},
		fixedNow,
	)
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	if err := repo.Add(t.Context(), b); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return b
}

// seedBatchExpiryStock creates a live Batch with an explicit expiry date then
// stocks it via an Inbound movement (so FEFO ordering + available are testable).
func seedBatchExpiryStock(t *testing.T, repo *batchtest.FakeRepository, p *product.Product, actor membership.ID, batchNumber string, expiry time.Time, qty int64) *batch.Batch {
	t.Helper()
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, err := batch.New(
		batch.ID(ids.NewV7().String()),
		p.ID(), p.TenantID(), actor,
		batch.Spec{
			BatchNumber:                batchNumber,
			ManufactureDate:            mfg,
			ExpiryDate:                 expiry,
			ManufacturerName:           "Acme",
			ManufacturingLicenceNumber: "ML-1",
			MRPPaise:                   25000,
			PurchasePricePaise:         18000,
		},
		fixedNow,
	)
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	if qty > 0 {
		if merr := b.ApplyMovement(batch.MovementInbound, qty, fixedNow, fixedNow); merr != nil {
			t.Fatalf("seed inbound: %v", merr)
		}
	}
	if err := repo.Add(t.Context(), b); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return b
}

// seedBatchWithStock seeds a batch (default 2028 expiry) carrying qty on-hand.
func seedBatchWithStock(t *testing.T, repo *batchtest.FakeRepository, p *product.Product, actor membership.ID, batchNumber string, qty int64) *batch.Batch {
	t.Helper()
	return seedBatchExpiryStock(t, repo, p, actor, batchNumber, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), qty)
}

// seedBatchLaterExpiryWithStock seeds a batch with a 2029 expiry (sorts AFTER a
// default 2028 batch under FEFO) carrying qty on-hand.
func seedBatchLaterExpiryWithStock(t *testing.T, repo *batchtest.FakeRepository, p *product.Product, actor membership.ID, batchNumber string, qty int64) *batch.Batch {
	t.Helper()
	return seedBatchExpiryStock(t, repo, p, actor, batchNumber, time.Date(2029, 1, 31, 0, 0, 0, 0, time.UTC), qty)
}

// countMovements counts ledger rows of a given movement type in the fake.
func countMovements(repo *stockmovementtest.FakeRepository, mt batch.MovementType) int {
	n := 0
	for _, m := range repo.Movements {
		if m.Type() == mt {
			n++
		}
	}
	return n
}
