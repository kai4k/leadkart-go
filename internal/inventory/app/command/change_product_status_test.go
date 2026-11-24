package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ADR 0073: deactivate and reactivate move the product aggregate and nothing else; each is
// idempotent, a missing product is ErrNotFound, and nothing is deleted.
func TestChangeProductStatusHandlers(t *testing.T) {
	t.Parallel()
	repo := newFakeProductRepo()
	tid := newTenantID(t)
	actor := newMembershipID(t)
	p := seedProduct(t, repo, tid, actor, "ST-1")
	now := func() time.Time { return fixedNow }
	deactivate := command.NewDeactivateProductHandler(repo, now)
	reactivate := command.NewReactivateProductHandler(repo, now)
	cmd := command.ChangeProductStatusCommand{TenantID: tid, ProductID: p.ID(), ActorMembershipID: actor}

	if err := deactivate.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if p.IsActive() {
		t.Fatal("product still active after deactivate")
	}
	if err := deactivate.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("second deactivate must be a no-op: %v", err)
	}
	got, err := repo.GetByID(t.Context(), tid, p.ID())
	if err != nil || got.IsActive() {
		t.Fatalf("an inactive product must stay readable: %v active=%v", err, got != nil && got.IsActive())
	}

	if err := reactivate.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if !p.IsActive() {
		t.Fatal("product still inactive after reactivate")
	}
	if err := reactivate.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("second reactivate must be a no-op: %v", err)
	}

	missing := command.ChangeProductStatusCommand{TenantID: tid, ProductID: product.ID("11111111-1111-1111-1111-111111111111"), ActorMembershipID: actor}
	if err := deactivate.Handle(t.Context(), missing); !errors.Is(err, product.ErrNotFound) {
		t.Fatalf("missing product: %v, want ErrNotFound", err)
	}
	if err := deactivate.Handle(t.Context(), command.ChangeProductStatusCommand{ProductID: p.ID(), ActorMembershipID: actor}); err == nil {
		t.Fatal("a missing tenant must be refused")
	}
}
