package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestCreateProductHandler_HappyPath_Persists(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo)
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
	if repo.addCalls != 1 {
		t.Fatalf("addCalls: got %d want 1", repo.addCalls)
	}
}

// Failure 1: invariant violation (empty SKU) — ErrInvalid surfaces.
func TestCreateProductHandler_RejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo)
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
	if repo.addCalls != 0 {
		t.Fatalf("addCalls on invalid spec: got %d want 0 (Add MUST NOT be called)", repo.addCalls)
	}
}

// Failure 2: duplicate SKU → ErrSKUTaken propagates.
func TestCreateProductHandler_DuplicateSKU_ReturnsErrSKUTaken(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewCreateProductHandler(repo)
	tid := newTenantID(t)
	actor := newMembershipID(t)
	_ = seedProduct(t, repo, tid, actor, "DUP-1")

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
	repo.addErr = errSentinel
	h := command.NewCreateProductHandler(repo)
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
