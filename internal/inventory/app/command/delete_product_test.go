package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestDeleteProductHandler_HappyPath_SoftDeletes(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "DEL-1")
	h := command.NewDeleteProductHandler(productRepo, batchRepo, func() time.Time { return fixedNow })

	if err := h.Handle(t.Context(), command.DeleteProductCommand{
		ProductID: p.ID(), ActorMembershipID: actor,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Re-load via direct map access — fakeProductRepo.GetByID filters
	// deleted rows; we want to confirm the soft-delete was APPLIED.
	if !p.IsDeleted() {
		t.Fatal("product should be soft-deleted after Handle")
	}
}

// Failure 1: missing product → ErrNotFound — per ADR 0061 amendment 1
// (M6) the handler does an upfront GetByID, surfacing this directly.
func TestDeleteProductHandler_MissingProduct_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	h := command.NewDeleteProductHandler(productRepo, batchRepo, func() time.Time { return fixedNow })
	actor := newMembershipID(t)

	err := h.Handle(t.Context(), command.DeleteProductCommand{
		ProductID: product.ID("nonexistent"), ActorMembershipID: actor,
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

// Failure 2: live batches with stock → ErrAnyLiveStock.
func TestDeleteProductHandler_HasLiveStock_ReturnsErrAnyLiveStock(t *testing.T) {
	t.Parallel()
	productRepo := newFakeProductRepo()
	batchRepo := newFakeBatchRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, productRepo, tid, actor, "STOCK-1")
	batchRepo.AnyLiveStockFor = p.ID()
	batchRepo.AnyLiveStockOn = true
	h := command.NewDeleteProductHandler(productRepo, batchRepo, func() time.Time { return fixedNow })

	err := h.Handle(t.Context(), command.DeleteProductCommand{
		ProductID: p.ID(), ActorMembershipID: actor,
	})
	if !errors.Is(err, batch.ErrAnyLiveStock) {
		t.Fatalf("err: got %v want ErrAnyLiveStock", err)
	}
	if p.IsDeleted() {
		t.Fatal("product MUST NOT be deleted when live stock exists")
	}
}
