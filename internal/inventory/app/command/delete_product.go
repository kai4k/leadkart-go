package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// DeleteProductCommand soft-deletes the product. Per BRD §6.5 it is rejected
// with batch.ErrAnyLiveStock if any live batch has quantity_on_hand > 0.
//
// TenantID is the caller's tenant scope; per ADR 0062 (TDL canon) it flows
// through an explicit field, never via context smuggling.
type DeleteProductCommand struct {
	TenantID          tenant.ID
	ProductID         product.ID
	ActorMembershipID membership.ID
}

// DeleteProductHandler enforces the no-live-stock guard before soft-deleting
// the Product. The cross-aggregate read lives in the handler per Vernon
// ch.10; the mutation stays inside Product.SoftDelete on a single-aggregate tx.
type DeleteProductHandler struct {
	products product.Repository
	batches  batch.Repository
	now      func() time.Time
}

// NewDeleteProductHandler wires the handler. now is the injected clock (nil →
// time.Now).
func NewDeleteProductHandler(products product.Repository, batches batch.Repository, now func() time.Time) DeleteProductHandler {
	if now == nil {
		now = time.Now
	}
	return DeleteProductHandler{products: products, batches: batches, now: now}
}

// Handle runs the stock guard, then soft-deletes the product.
//
// Per ADR 0061 amendment 1 (M6): GetByID runs first so a missing product
// surfaces as product.ErrNotFound (mapped to 204 idempotent-delete) instead
// of an extra round-trip through the stock check and SoftDelete.
func (h DeleteProductHandler) Handle(ctx context.Context, cmd DeleteProductCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("delete_product: tenant id required")
	}
	if _, err := h.products.GetByID(ctx, cmd.TenantID, cmd.ProductID); err != nil {
		return err
	}
	hasStock, err := h.batches.AnyLiveWithStockForProduct(ctx, cmd.TenantID, cmd.ProductID)
	if err != nil {
		return fmt.Errorf("delete product: stock check: %w", err)
	}
	if hasStock {
		return batch.ErrAnyLiveStock
	}
	now := h.now()
	err = h.products.UpdateByID(ctx, cmd.TenantID, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.SoftDelete(cmd.ActorMembershipID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("delete product: %w", err)
	}
	return nil
}
