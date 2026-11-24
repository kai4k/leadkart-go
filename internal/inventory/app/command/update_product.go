package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
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
	GSTRateID         *string
	Manufacturer      *string
	// ReorderLevel, ExpiryAlertThresholdDays and ProductCategory are the
	// product form's alert and catalogue fields. A blank ProductCategory is
	// stored as the default category ([product.ProductCategoryDefault]).
	ReorderLevel             *int
	ExpiryAlertThresholdDays *int
	ProductCategory          *string
	DefaultSaleRatePaise     *int64
	GenericName              *string
	Composition              *string
	StorageCondition         *string
	// DrugSchedule is the class id; a pointer to "" clears it. nil leaves it alone.
	DrugSchedule *string
}

// UpdateProductHandler runs load → updateFn → persist plus event drain under
// the repository's tx (TDL UpdateFn pattern, ADR 0004).
type UpdateProductHandler struct {
	products product.Repository
	policy   sellability.PolicyReader
	now      func() time.Time
}

// NewUpdateProductHandler wires the handler. now is the injected clock (nil →
// time.Now).
func NewUpdateProductHandler(products product.Repository, policy sellability.PolicyReader, now func() time.Time) UpdateProductHandler {
	if policy == nil {
		panic("command: NewUpdateProductHandler policy required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateProductHandler{products: products, policy: policy, now: now}
}

// Handle applies the partial update. ErrNotFound → 404, product.ErrInvalid →
// 422 (with field detail at the HTTP layer).
func (h UpdateProductHandler) Handle(ctx context.Context, cmd UpdateProductCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_product: tenant id required")
	}
	now := h.now()
	// The company lists are read BEFORE the product row is locked (ReadPolicy joins a
	// caller transaction when there is one; here there is none).
	pol, err := h.policy.ReadPolicy(ctx, cmd.TenantID)
	if err != nil {
		return fmt.Errorf("update product: read policy: %w", err)
	}
	err = h.products.UpdateByID(ctx, cmd.TenantID, cmd.ProductID, func(p *product.Product) (bool, error) {
		var rate *int
		if cmd.GSTRateID != nil || cmd.DrugSchedule != nil {
			if cmd.GSTRateID != nil && *cmd.GSTRateID == "" {
				return false, &FieldRefusal{Fields: map[string][]string{"gst_rate_id": {CodeRequired}}}
			}
			bps, refusal := resolveCatalogue(pol.Policy, catalogueChoice{
				CurrentGSTRateID: p.GSTRateID(), GSTRateID: derefString(cmd.GSTRateID),
				ScheduleGiven: cmd.DrugSchedule != nil, DrugSchedule: derefString(cmd.DrugSchedule),
			})
			if refusal != nil {
				return false, refusal
			}
			if bps >= 0 {
				rate = &bps
			}
		}
		if err := p.Update(cmd.ActorMembershipID, product.UpdateSpec{
			Name:                     cmd.Name,
			GSTRateBps:               rate,
			GSTRateID:                cmd.GSTRateID,
			DefaultSaleRatePaise:     cmd.DefaultSaleRatePaise,
			GenericName:              cmd.GenericName,
			Composition:              cmd.Composition,
			StorageCondition:         cmd.StorageCondition,
			DrugScheduleClassID:      cmd.DrugSchedule,
			Manufacturer:             cmd.Manufacturer,
			ReorderLevel:             cmd.ReorderLevel,
			ExpiryAlertThresholdDays: cmd.ExpiryAlertThresholdDays,
			ProductCategory:          cmd.ProductCategory,
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

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
