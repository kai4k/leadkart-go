package command

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// UpdateProductCommand — partial-update payload. Only non-nil fields
// are considered; matching-value updates are no-op (no event).
type UpdateProductCommand struct {
	ProductID         product.ID
	ActorMembershipID membership.ID
	Name              *string
	GSTRateBps        *int
	IsActive          *bool
	Manufacturer      *string
}

// UpdateProductHandler runs Load → updateFn → Persist + event drain
// under the repository's tx (TDL UpdateFn pattern per ADR 0004).
type UpdateProductHandler struct {
	products product.Repository
}

// NewUpdateProductHandler wires the handler.
func NewUpdateProductHandler(products product.Repository) UpdateProductHandler {
	return UpdateProductHandler{products: products}
}

// Handle applies the partial update. ErrNotFound surfaces as 404.
// product.ErrInvalid surfaces as 422 (with field detail in the inline
// HTTP handler). product.ErrDeleted surfaces as 409.
func (h UpdateProductHandler) Handle(ctx context.Context, cmd UpdateProductCommand) error {
	err := h.products.UpdateByID(ctx, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.Update(cmd.ActorMembershipID, product.UpdateSpec{
			Name:         cmd.Name,
			GSTRateBps:   cmd.GSTRateBps,
			IsActive:     cmd.IsActive,
			Manufacturer: cmd.Manufacturer,
		}); err != nil {
			return false, err
		}
		// Always persist — Product.Update is idempotent (no-op when
		// nothing changed; no event drained). A redundant UPDATE on a
		// no-op call is the same shape identity's tenant handlers
		// accept; cheap on the hot path + keeps the handler symmetrical.
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("update product: %w", err)
	}
	return nil
}
