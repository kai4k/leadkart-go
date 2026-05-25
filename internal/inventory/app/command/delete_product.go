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

// DeleteProductCommand — soft-deletes the product. Per BRD §6.5 + slice
// spec: REJECTED with batch.ErrAnyLiveStock if any live batch with
// quantity_on_hand > 0 exists.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). Per ADR 0062 (TDL canon): tenantID flows through
// explicit command fields, not via context smuggling.
type DeleteProductCommand struct {
	TenantID          tenant.ID
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
	now      func() time.Time
}

// NewDeleteProductHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor — composition root wires
// `time.Now`; tests inject a fixed-time closure. Nil → time.Now.
func NewDeleteProductHandler(products product.Repository, batches batch.Repository, now func() time.Time) DeleteProductHandler {
	if now == nil {
		now = time.Now
	}
	return DeleteProductHandler{products: products, batches: batches, now: now}
}

// Handle runs the stock guard, then soft-deletes the product.
//
// Per ADR 0061 amendment 1 (M6): GetByID runs FIRST so a missing
// product surfaces as the friendlier `product.ErrNotFound` (mapped to
// 204 idempotent-delete by the HTTP handler) rather than the
// stock-check path returning false → the SoftDelete UpdateByID then
// returning ErrNotFound after an unnecessary round-trip.
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
