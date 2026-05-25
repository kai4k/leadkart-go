package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestAddBatchHandler_HappyPath_AddsBatch(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-1")
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, func() time.Time { return fixedNow }, testNewBatchID)

	out, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-1",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.BatchID.IsZero() {
		t.Fatal("BatchID should be set on success")
	}
	if uow.Runs() != 1 {
		t.Fatalf("UoW runs: got %d want 1", uow.Runs())
	}
	if batchRepo.AddCalls != 1 {
		t.Fatalf("batchRepo.AddCalls: got %d want 1", batchRepo.AddCalls)
	}
}

// Failure 1: missing product → ErrNotFound.
func TestAddBatchHandler_MissingProduct_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, func() time.Time { return fixedNow }, testNewBatchID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  product.ID("nonexistent"),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-1",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
	if batchRepo.AddCalls != 0 {
		t.Fatalf("batchRepo.AddCalls on missing product: got %d want 0", batchRepo.AddCalls)
	}
}

// Failure 2: soft-deleted parent product (race re-check) → ErrNotFound.
// Per ADR 0061 amendment 1 H4: AddBatch wraps both calls in a UoW + the
// inner re-check fires on a parent that became soft-deleted between
// the start of the tx and the GetByID. fakeProductRepo.GetByID already
// filters IsDeleted, so the GetByID itself returns ErrNotFound — same
// observable result, the H4 fix is structurally enforced by the UoW
// wrapping (not just the explicit re-check, which is defence-in-depth).
func TestAddBatchHandler_SoftDeletedParent_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-2")
	// Soft-delete BEFORE the handler runs.
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_ = p.PullEvents()
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, func() time.Time { return fixedNow }, testNewBatchID)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-X",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound (soft-deleted parent)", err)
	}
	if batchRepo.AddCalls != 0 {
		t.Fatalf("batchRepo.AddCalls on deleted parent: got %d want 0", batchRepo.AddCalls)
	}
}

// Failure 3: duplicate batch_number → ErrBatchNumberTaken.
func TestAddBatchHandler_DuplicateBatchNumber_ReturnsErrBatchNumberTaken(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-3")
	_ = seedBatch(t, batchRepo, p, actor, "DUP-LOT") // arch-test:ignore-err — seed helper, error reported via t.Fatalf inside helper
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, func() time.Time { return fixedNow }, testNewBatchID)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "DUP-LOT",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if !errors.Is(err, batch.ErrBatchNumberTaken) {
		t.Fatalf("err: got %v want ErrBatchNumberTaken", err)
	}
}

// Failure 4: invalid spec (zero MRP is permitted, but expiry == manufacture
// fails the chk_batch_expiry_after_manufacture invariant).
func TestAddBatchHandler_InvalidSpec_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-4")
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, func() time.Time { return fixedNow }, testNewBatchID)
	sameDay := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-1",
		ManufactureDate:            sameDay,
		ExpiryDate:                 sameDay,
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if !errors.Is(err, batch.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
}
