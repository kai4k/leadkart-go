package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ChangeProductStatusCommand moves a product between active and inactive. A product is never
// deleted (ADR 0073): it stays readable and keeps its batches and stock; an inactive product
// takes no new batch.
//
// TenantID is the caller's tenant scope, explicit per ADR 0062.
type ChangeProductStatusCommand struct {
	TenantID          tenant.ID
	ProductID         product.ID
	ActorMembershipID membership.ID
}

// DeactivateProductHandler makes a product inactive. Idempotent: an inactive product is a no-op
// (no event). The existing orders, batches and stock are untouched.
type DeactivateProductHandler struct {
	products product.Repository
	now      func() time.Time
}

// NewDeactivateProductHandler wires the handler. now is the injected clock (nil: time.Now).
func NewDeactivateProductHandler(products product.Repository, now func() time.Time) DeactivateProductHandler {
	if now == nil {
		now = time.Now
	}
	return DeactivateProductHandler{products: products, now: now}
}

// Handle runs the transition on the product aggregate: product.ErrNotFound for a missing product.
func (h DeactivateProductHandler) Handle(ctx context.Context, cmd ChangeProductStatusCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("deactivate_product: tenant id required")
	}
	now := h.now()
	if err := h.products.UpdateByID(ctx, cmd.TenantID, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.Deactivate(cmd.ActorMembershipID, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("deactivate product: %w", err)
	}
	return nil
}

// ReactivateProductHandler makes an inactive product active again. Idempotent.
type ReactivateProductHandler struct {
	products product.Repository
	now      func() time.Time
}

// NewReactivateProductHandler wires the handler. now is the injected clock (nil: time.Now).
func NewReactivateProductHandler(products product.Repository, now func() time.Time) ReactivateProductHandler {
	if now == nil {
		now = time.Now
	}
	return ReactivateProductHandler{products: products, now: now}
}

// Handle runs the transition on the product aggregate: product.ErrNotFound for a missing product.
func (h ReactivateProductHandler) Handle(ctx context.Context, cmd ChangeProductStatusCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("reactivate_product: tenant id required")
	}
	now := h.now()
	if err := h.products.UpdateByID(ctx, cmd.TenantID, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.Activate(cmd.ActorMembershipID, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("reactivate product: %w", err)
	}
	return nil
}
