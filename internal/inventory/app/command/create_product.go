// Package command holds Inventory CQRS command handlers.
//
// Per TDL Wild Workouts canon each handler is the orchestrator and the
// contract: a concrete struct with one Handle method, called directly via
// app.Commands.X.Handle(...). Per ADR 0047 handlers depend on domain
// repository interfaces only — never pgx or concrete adapters.
package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// CreateProductCommand is the validated input for creating a Product.
// ActorMembershipID populates CreatedByMembershipID on the audit event.
type CreateProductCommand struct {
	TenantID          tenant.ID
	ActorMembershipID membership.ID
	SKU               string
	Name              string
	DosageForm        string
	PackSize          string
	HSNCode           string
	GSTRateBps        int
	Manufacturer      string
}

// CreateProductResult carries the new ProductID.
type CreateProductResult struct {
	ProductID product.ID
}

// CreateProductHandler does a single-aggregate insert plus outbox drain.
// ErrSKUTaken surfaces as HTTP 409.
type CreateProductHandler struct {
	products     product.Repository
	now          func() time.Time
	newProductID func() product.ID
}

// NewCreateProductHandler wires the handler. now is the injected clock (nil →
// time.Now). newProductID is the aggregate-ID factory required by
// TestArch_HandlersInjectIDFactory; tests inject a deterministic one.
func NewCreateProductHandler(products product.Repository, now func() time.Time, newProductID func() product.ID) CreateProductHandler {
	if newProductID == nil {
		panic("command: NewCreateProductHandler newProductID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateProductHandler{products: products, now: now, newProductID: newProductID}
}

// Handle constructs and persists the Product. The outbox drain rides the
// adapter's Add path in one tx with the row insert (ADR 0008).
func (h CreateProductHandler) Handle(ctx context.Context, cmd CreateProductCommand) (CreateProductResult, error) {
	p, err := product.New(
		h.newProductID(),
		cmd.TenantID,
		cmd.ActorMembershipID,
		product.Spec{
			SKU:          cmd.SKU,
			Name:         cmd.Name,
			DosageForm:   cmd.DosageForm,
			PackSize:     cmd.PackSize,
			HSNCode:      cmd.HSNCode,
			GSTRateBps:   cmd.GSTRateBps,
			Manufacturer: cmd.Manufacturer,
		},
		h.now(),
	)
	if err != nil {
		return CreateProductResult{}, fmt.Errorf("create product: construct: %w", err)
	}
	if err := h.products.Add(ctx, p); err != nil {
		return CreateProductResult{}, fmt.Errorf("create product: persist: %w", err)
	}
	return CreateProductResult{ProductID: p.ID()}, nil
}
