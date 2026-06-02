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

// UpdateProductCommand is a partial-update payload: only non-nil fields apply,
// and a matching-value update is a no-op (no event).
//
// TenantID is the caller's tenant scope; per ADR 0062 (TDL canon) it flows
// through an explicit field, never via context smuggling. The adapter binds
// the GUC from it at tx-begin; RLS remains the security backstop.
type UpdateProductCommand struct {
	TenantID          tenant.ID
	ProductID         product.ID
	ActorMembershipID membership.ID
	Name              *string
	GSTRateBps        *int
	IsActive          *bool
	Manufacturer      *string
}

// UpdateProductHandler runs load → updateFn → persist plus event drain under
// the repository's tx (TDL UpdateFn pattern, ADR 0004).
type UpdateProductHandler struct {
	products product.Repository
	now      func() time.Time
}

// NewUpdateProductHandler wires the handler. now is the injected clock (nil →
// time.Now).
func NewUpdateProductHandler(products product.Repository, now func() time.Time) UpdateProductHandler {
	if now == nil {
		now = time.Now
	}
	return UpdateProductHandler{products: products, now: now}
}

// Handle applies the partial update. ErrNotFound → 404, product.ErrInvalid →
// 422 (with field detail at the HTTP layer), product.ErrDeleted → 409.
func (h UpdateProductHandler) Handle(ctx context.Context, cmd UpdateProductCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_product: tenant id required")
	}
	now := h.now()
	err := h.products.UpdateByID(ctx, cmd.TenantID, cmd.ProductID, func(p *product.Product) (bool, error) {
		if err := p.Update(cmd.ActorMembershipID, product.UpdateSpec{
			Name:         cmd.Name,
			GSTRateBps:   cmd.GSTRateBps,
			IsActive:     cmd.IsActive,
			Manufacturer: cmd.Manufacturer,
		}, now); err != nil {
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
