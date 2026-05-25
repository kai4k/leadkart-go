package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestCreateProductHandler_HappyPath_Persists(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	out, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "AMOX-500", Name: "Amoxicillin 500 mg",
		DosageForm: "Capsule", PackSize: "10x10",
		HSNCode: "30049099", GSTRateBps: 1200,
		Manufacturer: "Acme",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.ProductID.IsZero() {
		t.Fatal("ProductID should be set on success")
	}
	if repo.AddCalls != 1 {
		t.Fatalf("AddCalls: got %d want 1", repo.AddCalls)
	}
}

// Failure 1: invariant violation (empty SKU) — ErrInvalid surfaces.
func TestCreateProductHandler_RejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "", Name: "x", // empty SKU → ErrInvalid
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: 1200,
	})
	if !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
	if repo.AddCalls != 0 {
		t.Fatalf("AddCalls on invalid spec: got %d want 0 (Add MUST NOT be called)", repo.AddCalls)
	}
}

// Failure 2: duplicate SKU → ErrSKUTaken propagates.
func TestCreateProductHandler_DuplicateSKU_ReturnsErrSKUTaken(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)
	_ = seedProduct(t, repo, tid, actor, "DUP-1") // arch-test:ignore-err — seed helper, error reported via t.Fatalf inside helper

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "DUP-1", Name: "Second",
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: 1200,
	})
	if !errors.Is(err, product.ErrSKUTaken) {
		t.Fatalf("err: got %v want ErrSKUTaken", err)
	}
}

// Failure 3: repo Add error propagates (non-domain error surfaces as
// wrapped err — caller can branch on errors.Is).
func TestCreateProductHandler_RepoAddError_Propagates(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	repo.AddErr = errSentinel
	h := command.NewCreateProductHandler(repo, func() time.Time { return fixedNow }, testNewProductID)
	tid := newTenantID(t)
	actor := newMembershipID(t)

	_, err := h.Handle(t.Context(), command.CreateProductCommand{
		TenantID: tid, ActorMembershipID: actor,
		SKU: "X-1", Name: "X",
		DosageForm: "Tablet", PackSize: "10",
		HSNCode: "3004", GSTRateBps: 1200,
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err: got %v want errSentinel propagated", err)
	}
}
