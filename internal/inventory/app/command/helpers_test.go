package command_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func newTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func newMembershipID(t *testing.T) membership.ID {
	t.Helper()
	return membership.ID(ids.NewV7().String())
}

// seedProduct creates + Adds a fresh Product to repo, returning it. SKU
// is supplied per call so tests can drive ErrSKUTaken paths.
func seedProduct(t *testing.T, repo *fakeProductRepo, tid tenant.ID, actor membership.ID, sku string) *product.Product {
	t.Helper()
	p, err := product.New(
		product.ID(ids.NewV7().String()),
		tid, actor,
		product.Spec{
			SKU: sku, Name: "Test Drug",
			DosageForm: "Tablet", PackSize: "10",
			HSNCode: "3004", GSTRateBps: 1200,
		},
	)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return p
}

// seedBatch creates + Adds a fresh live Batch to repo for the given
// product. Batch starts with quantity_on_hand=0.
func seedBatch(t *testing.T, repo *fakeBatchRepo, p *product.Product, actor membership.ID, batchNumber string) *batch.Batch {
	t.Helper()
	mfg := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)
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
	)
	if err != nil {
		t.Fatalf("batch.New: %v", err)
	}
	if err := repo.Add(t.Context(), b); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return b
}
