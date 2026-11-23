package command_test

import (
	"errors"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability/sellabilitytest"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
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
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	out, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-1",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
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

// Missing product → ErrNotFound.
func TestAddBatchHandler_MissingProduct_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  product.ID("nonexistent"),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-1",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
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

// Soft-deleted parent → ErrNotFound (ADR 0061 amendment 1 H4). The fake's
// GetByID filters IsDeleted, so it returns ErrNotFound directly; the H4 fix is
// the UoW wrapping, with the explicit re-check as defence-in-depth.
func TestAddBatchHandler_SoftDeletedParent_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-2")
	// Soft-delete before the handler runs.
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_ = p.PullEvents()
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "LOT-X",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
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

// Duplicate batch_number → ErrBatchNumberTaken.
func TestAddBatchHandler_DuplicateBatchNumber_ReturnsErrBatchNumberTaken(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-3")
	_ = seedBatch(t, batchRepo, p, actor, "DUP-LOT") // arch-test:ignore-err — seed helper, error reported via t.Fatalf inside helper
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	_, err := h.Handle(t.Context(), command.AddBatchCommand{
		TenantID:                   tid,
		ProductID:                  p.ID(),
		ActorMembershipID:          actor,
		BatchNumber:                "DUP-LOT",
		ManufactureDate:            time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:                 time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC),
		ManufacturerName:           "Acme",
		ManufacturingLicenceNumber: "ML-1",
		MRPPaise:                   25000,
		PurchasePricePaise:         18000,
	})
	if !errors.Is(err, batch.ErrBatchNumberTaken) {
		t.Fatalf("err: got %v want ErrBatchNumberTaken", err)
	}
}

// Invalid spec: expiry == manufacture fails the
// chk_batch_expiry_after_manufacture invariant.
func TestAddBatchHandler_InvalidSpec_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	uow := &fakeUoW{}
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "AB-4")
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)
	sameDay := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

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

// addBatchCmd is a valid AddBatch command expiring on expiry with the given opening stock.
func addBatchCmd(p *product.Product, actor membership.ID, expiry time.Time, initial int64) command.AddBatchCommand {
	return command.AddBatchCommand{
		TenantID: p.TenantID(), ProductID: p.ID(), ActorMembershipID: actor,
		BatchNumber:      "LOT-INIT",
		ManufactureDate:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ExpiryDate:       expiry,
		ManufacturerName: "Acme", ManufacturingLicenceNumber: "ML-1", MRPPaise: 25000, PurchasePricePaise: 18000,
		InitialQuantity: initial,
	}
}

// Audit C6: a batch can be added WITH its opening stock. The batch and the
// inbound ledger row are written in the one unit of work.
func TestAddBatchHandler_InitialQuantity_StocksTheBatchInTheSameUnitOfWork(t *testing.T) {
	t.Parallel()
	productRepo, batchRepo, movementRepo := newFakeProductRepo(), newFakeBatchRepo(), newFakeMovementRepo()
	uow := &fakeUoW{}
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-INIT")
	h := command.NewAddBatchHandler(uow, productRepo, batchRepo, movementRepo, sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	out, err := h.Handle(t.Context(), addBatchCmd(p, actor, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 25))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if uow.Runs() != 1 {
		t.Fatalf("UoW runs: got %d want 1 (batch and stock share one transaction)", uow.Runs())
	}
	b, err := batchRepo.GetByID(t.Context(), p.TenantID(), out.BatchID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if b.QuantityOnHand() != 25 || b.QuantityReserved() != 0 {
		t.Fatalf("on hand %d reserved %d; want 25 and 0", b.QuantityOnHand(), b.QuantityReserved())
	}
	if len(movementRepo.Movements) != 1 {
		t.Fatalf("ledger rows: got %d want 1", len(movementRepo.Movements))
	}
	for _, m := range movementRepo.Movements {
		if m.BatchID() != out.BatchID || m.Type() != batch.MovementInbound || m.Quantity() != 25 ||
			m.QuantityOnHandAfter() != 25 || m.Reason() != command.OpeningStockReason || m.ActorMembershipID() != actor {
			t.Fatalf("ledger row: batch %s type %s quantity %d after %d reason %q actor %s",
				m.BatchID(), m.Type(), m.Quantity(), m.QuantityOnHandAfter(), m.Reason(), m.ActorMembershipID())
		}
	}
}

// Without an opening quantity nothing changes: an empty batch and no ledger row.
func TestAddBatchHandler_NoInitialQuantity_WritesNoMovement(t *testing.T) {
	t.Parallel()
	productRepo, batchRepo, movementRepo := newFakeProductRepo(), newFakeBatchRepo(), newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-NONE")
	h := command.NewAddBatchHandler(&fakeUoW{}, productRepo, batchRepo, movementRepo, sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	out, err := h.Handle(t.Context(), addBatchCmd(p, actor, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 0))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	b, _ := batchRepo.GetByID(t.Context(), p.TenantID(), out.BatchID)
	if b.QuantityOnHand() != 0 || b.Version() != 0 || len(movementRepo.Movements) != 0 {
		t.Fatalf("on hand %d version %d ledger rows %d; want 0, 0 and 0", b.QuantityOnHand(), b.Version(), len(movementRepo.Movements))
	}
}

// A refusal of the opening stock is decided BEFORE anything is written, so
// the batch cannot half-exist: a negative quantity, or stock for a batch that
// is already expired (the rule every inbound movement has).
func TestAddBatchHandler_InitialQuantity_RefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		expiry  time.Time
		initial int64
		want    error
	}{
		{"negative quantity", time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), -5, batch.ErrInvalid},
		{"expired batch", time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), 5, batch.ErrExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			productRepo, batchRepo, movementRepo := newFakeProductRepo(), newFakeBatchRepo(), newFakeMovementRepo()
			actor := newMembershipID(t)
			p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-REF")
			h := command.NewAddBatchHandler(&fakeUoW{}, productRepo, batchRepo, movementRepo, sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

			_, err := h.Handle(t.Context(), addBatchCmd(p, actor, c.expiry, c.initial))
			if !errors.Is(err, c.want) {
				t.Fatalf("err: got %v want %v", err, c.want)
			}
			if batchRepo.AddCalls != 0 || movementRepo.AddCalls != 0 {
				t.Fatalf("writes after a refusal: %d batch, %d ledger; want none", batchRepo.AddCalls, movementRepo.AddCalls)
			}
		})
	}
}

// An expired batch WITHOUT opening stock is still accepted, as before.
func TestAddBatchHandler_ExpiredBatchWithoutStock_StillAccepted(t *testing.T) {
	t.Parallel()
	productRepo, batchRepo := newFakeProductRepo(), newFakeBatchRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-OLD")
	h := command.NewAddBatchHandler(&fakeUoW{}, productRepo, batchRepo, newFakeMovementRepo(), sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	if _, err := h.Handle(t.Context(), addBatchCmd(p, actor, time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// A batch cannot have been made tomorrow: the manufacture date is judged against
// the company business day, and a refusal writes nothing.
func TestAddBatchHandler_ManufactureDateInTheFutureIsRefused(t *testing.T) {
	t.Parallel()
	productRepo, batchRepo, movementRepo := newFakeProductRepo(), newFakeBatchRepo(), newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-FUT")
	h := command.NewAddBatchHandler(&fakeUoW{}, productRepo, batchRepo, movementRepo, sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	cmd := addBatchCmd(p, actor, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 0)
	cmd.ManufactureDate = time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC) // the day after fixedNow's business day
	if _, err := h.Handle(t.Context(), cmd); !errors.Is(err, batch.ErrManufactureDateInFuture) {
		t.Fatalf("err = %v, want ErrManufactureDateInFuture", err)
	}
	if batchRepo.AddCalls != 0 {
		t.Fatal("a refused batch was written")
	}
	cmd.ManufactureDate = time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC) // today is fine
	if _, err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("manufactured today: %v", err)
	}
}

// Stock received into a batch already past the company cut-off (but not past its
// printed expiry) is accepted with a warning, never refused.
func TestAddBatchHandler_OpeningStockPastTheCutoffIsAcceptedWithAWarning(t *testing.T) {
	t.Parallel()
	productRepo, batchRepo, movementRepo := newFakeProductRepo(), newFakeBatchRepo(), newFakeMovementRepo()
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, newTenantID(t), actor, "AB-WARN")
	h := command.NewAddBatchHandler(&fakeUoW{}, productRepo, batchRepo, movementRepo, sellabilitytest.Resolver(fixedNow), func() time.Time { return fixedNow }, testNewBatchID, testNewMovementID)

	// fixedNow is 24 May 2026 (default stop: 1 month before). EXP 05/2026 is still
	// inside its printed month but past the cut-off.
	out, err := h.Handle(t.Context(), addBatchCmd(p, actor, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), 10))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(out.Warnings) != 1 || out.Warnings[0] != command.WarningPastSellableCutoff {
		t.Fatalf("warnings = %v", out.Warnings)
	}
	clean, err := h.Handle(t.Context(), func() command.AddBatchCommand {
		c := addBatchCmd(p, actor, time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), 10)
		c.BatchNumber = "LOT-OK"
		return c
	}())
	if err != nil || len(clean.Warnings) != 0 {
		t.Fatalf("a sellable batch warns nothing: %v %v", clean.Warnings, err)
	}
}
