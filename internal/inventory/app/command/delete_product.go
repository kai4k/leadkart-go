package command

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// DeleteProductCommand — soft-deletes the product. Per BRD §6.5 + slice
// spec: REJECTED with batch.ErrAnyLiveStock if any live batch with
// quantity_on_hand > 0 exists.
type DeleteProductCommand struct {
	ProductID         product.ID
	ActorMembershipID membership.ID
}

// DeleteProductHandler enforces the "no live stock" guard before
// soft-deleting the Product aggregate.
//
// Cross-aggregate read (batches.AnyLiveWithStockForProduct) lives in
// the handler per Vernon ch.10 — the domain doesn't reach across
// aggregates. The repository call is read-only; the actual mutation
// stays inside Product.SoftDelete on the single-aggregate tx.
type DeleteProductHandler struct {
	products product.Repository
	batches  batch.Repository
}

// NewDeleteProductHandler wires the handler.
func NewDeleteProductHandler(products product.Repository, batches batch.Repository) DeleteProductHandler {
	return DeleteProductHandler{products: products, batches: batches}
}

// Handle runs the stock guard, then soft-deletes the product.
func (h DeleteProductHandler) Handle(ctx context.Context, cmd DeleteProductCommand) error {
	hasStock, err := h.batches.AnyLiveWithStockForProduct(ctx, cmd.ProductID)
	if err != nil {
		return fmt.Errorf("delete product: stock check: %w", err)
	}
	if hasStock {
		return batch.ErrAnyLiveStock
	}
	err = h.products.UpdateByID(ctx, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.SoftDelete(cmd.ActorMembershipID); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("delete product: %w", err)
	}
	return nil
}
