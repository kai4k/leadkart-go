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

// UpdateProductCommand — partial-update payload. Only non-nil fields
// are considered; matching-value updates are no-op (no event).
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). Per ADR 0062 (TDL canon): tenantID flows through
// explicit command fields, not via context smuggling. The adapter binds
// the GUC from cmd.TenantID at tx-begin; RLS remains the security
// backstop.
type UpdateProductCommand struct {
	TenantID          tenant.ID
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
	now      func() time.Time
}

// NewUpdateProductHandler wires the handler. `now` is the explicit time
// source — composition root wires `time.Now`; tests inject a fixed-time
// closure. Nil → time.Now.
func NewUpdateProductHandler(products product.Repository, now func() time.Time) UpdateProductHandler {
	if now == nil {
		now = time.Now
	}
	return UpdateProductHandler{products: products, now: now}
}

// Handle applies the partial update. ErrNotFound surfaces as 404.
// product.ErrInvalid surfaces as 422 (with field detail in the inline
// HTTP handler). product.ErrDeleted surfaces as 409.
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
