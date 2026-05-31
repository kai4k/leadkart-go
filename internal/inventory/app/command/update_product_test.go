package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

func TestUpdateProductHandler_HappyPath_AppliesPartial(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "UPD-1")
	h := command.NewUpdateProductHandler(repo, func() time.Time { return fixedNow })

	newName := "Renamed"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor,
		Name: &newName,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := repo.GetByID(t.Context(), tid, p.ID())
	if got.Name() != newName {
		t.Fatalf("name: got %q want %q", got.Name(), newName)
	}
}

// Missing product → ErrNotFound (404 at HTTP).
func TestUpdateProductHandler_MissingProduct_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	h := command.NewUpdateProductHandler(repo, func() time.Time { return fixedNow })
	tid := newTenantID(t)
	actor := newMembershipID(t)

	newName := "x"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: product.ID("nonexistent"), ActorMembershipID: actor,
		Name: &newName,
	})
	if !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("err: got %v want ErrNotFound", err)
	}
}

// Out-of-range GST → ErrInvalid from the aggregate's Update guard.
func TestUpdateProductHandler_InvalidGSTRate_ReturnsErrInvalid(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "GST-1")
	h := command.NewUpdateProductHandler(repo, func() time.Time { return fixedNow })

	bad := 99999 // > 10000 ceiling
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor,
		GSTRateBps: &bad,
	})
	if !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("err: got %v want ErrInvalid", err)
	}
}

// Update on soft-deleted product → ErrDeleted.
func TestUpdateProductHandler_DeletedProduct_ReturnsErrDeleted(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "DEL-1")
	// Soft-delete via the domain; GetByID filters IsDeleted so we mutate
	// the aggregate directly.
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_ = p.PullEvents()
	h := command.NewUpdateProductHandler(repo, func() time.Time { return fixedNow })

	newName := "Late"
	err := h.Handle(t.Context(), command.UpdateProductCommand{
		TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor,
		Name: &newName,
	})
	// Product.Update returns ErrDeleted on a soft-deleted aggregate; the
	// HTTP layer maps it to 409.
	if !errors.Is(err, product.ErrDeleted) {
		t.Fatalf("err on update of soft-deleted product: got %v want ErrDeleted", err)
	}
}
