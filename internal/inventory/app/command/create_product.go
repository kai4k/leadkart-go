// Package command holds Inventory CQRS command handlers.
//
// Per TDL Wild Workouts canon: handler is the orchestrator AND
// contract; no service abstraction; each Handler is a concrete struct
// with a single Handle method. HTTP ports call
// `app.Commands.X.Handle(...)` directly.
//
// Boundary discipline (ADR 0047): handlers depend on domain repository
// INTERFACES only — no pgx, no concrete adapter structs, no
// adapters/db row types. The composition root wires concrete adapters.
package command

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// CreateProductCommand carries the validated input for creating a new
// Product. ActorMembershipID populates the CreatedByMembershipID on the
// integration event for audit.
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

// CreateProductHandler — single-aggregate insert + outbox drain.
// Wire-contract: ErrSKUTaken surfaces as HTTP 409.
type CreateProductHandler struct {
	products product.Repository
	now      func() time.Time
}

// NewCreateProductHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor — composition root wires
// `time.Now`; tests inject a fixed-time closure for deterministic
// timestamps. Nil → time.Now.
func NewCreateProductHandler(products product.Repository, now func() time.Time) CreateProductHandler {
	if now == nil {
		now = time.Now
	}
	return CreateProductHandler{products: products, now: now}
}

// Handle constructs + persists the Product. Outbox event drain rides
// the adapter's Add path (one-tx with the row insert per ADR 0008).
func (h CreateProductHandler) Handle(ctx context.Context, cmd CreateProductCommand) (CreateProductResult, error) {
	p, err := product.New(
		product.ID(ids.NewV7().String()),
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
