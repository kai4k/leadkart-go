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

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// CreateProductCommand carries the validated input for creating a new
// Product. ActorMembershipID populates the CreatedByMembershipID on the
// integration event for audit.
//
// GSTRateBps: when 0 AND a GstDefaultReader is wired AND ProductCategory
// resolves to a seeded row in shared.product_category_gst_defaults, the
// handler substitutes the default value. Non-zero values bypass lookup.
//
// ReorderLevel / ExpiryAlertThresholdDays / ProductCategory carry their
// canonical defaults (0 / 90 / "General") when omitted.
type CreateProductCommand struct {
	TenantID                 tenant.ID
	ActorMembershipID        membership.ID
	SKU                      string
	Name                     string
	DosageForm               string
	PackSize                 string
	HSNCode                  string
	GSTRateBps               int
	Manufacturer             string
	ReorderLevel             int
	ExpiryAlertThresholdDays int
	ProductCategory          string
}

// CreateProductResult carries the new ProductID.
type CreateProductResult struct {
	ProductID product.ID
}

// GstDefaultReader is the handler-local interface for resolving the
// per-category default GST rate (basis points) when the caller passes
// GSTRateBps == 0. The concrete reader lives in
// `internal/common/refdata/` (see [refdata.GstDefaultReader]).
//
// Handler-local interface (TDL Wild Workouts canon): consumer-side, no
// cross-module import of the concrete adapter package.
type GstDefaultReader interface {
	// Default returns the GST rate in basis points for category, the
	// `found` flag, or (0, false, nil) when no row matches. Errors
	// surface only on infrastructure failure (cache + DB both down).
	Default(ctx context.Context, category string) (int, bool, error)
}

// CreateProductHandler — single-aggregate insert + outbox drain.
// Wire-contract: ErrSKUTaken surfaces as HTTP 409.
//
// gstDefaults is OPTIONAL: when non-nil, the handler resolves a default
// GST rate from the shared.product_category_gst_defaults reference table
// for any submission with GSTRateBps == 0.
type CreateProductHandler struct {
	products     product.Repository
	gstDefaults  GstDefaultReader
	now          func() time.Time
	newProductID func() product.ID
}

// NewCreateProductHandler wires the handler. `now` is the explicit time
// source — composition root wires `time.Now`; tests inject a fixed-time
// closure. Nil → time.Now.
//
// newProductID is the aggregate-ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline.
//
// gstDefaults may be nil (no per-category lookup; caller supplies GST).
func NewCreateProductHandler(products product.Repository, gstDefaults GstDefaultReader, now func() time.Time, newProductID func() product.ID) CreateProductHandler {
	if newProductID == nil {
		panic("command: NewCreateProductHandler newProductID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateProductHandler{products: products, gstDefaults: gstDefaults, now: now, newProductID: newProductID}
}

// Handle constructs + persists the Product. Outbox event drain rides
// the adapter's Add path (one-tx with the row insert per ADR 0008).
//
// GST default fallback: when cmd.GSTRateBps == 0 AND a GstDefaultReader
// is wired, the handler looks up the category-driven default.
func (h CreateProductHandler) Handle(ctx context.Context, cmd CreateProductCommand) (CreateProductResult, error) {
	gst := cmd.GSTRateBps
	if gst == 0 && h.gstDefaults != nil {
		category := cmd.ProductCategory
		if category == "" {
			category = product.ProductCategoryDefault
		}
		def, found, err := h.gstDefaults.Default(ctx, category)
		if err != nil {
			return CreateProductResult{}, fmt.Errorf("create product: gst default lookup: %w", err)
		}
		if found {
			gst = def
		}
	}
	p, err := product.New(
		h.newProductID(),
		cmd.TenantID,
		cmd.ActorMembershipID,
		product.Spec{
			SKU:                      cmd.SKU,
			Name:                     cmd.Name,
			DosageForm:               cmd.DosageForm,
			PackSize:                 cmd.PackSize,
			HSNCode:                  cmd.HSNCode,
			GSTRateBps:               gst,
			Manufacturer:             cmd.Manufacturer,
			ReorderLevel:             cmd.ReorderLevel,
			ExpiryAlertThresholdDays: cmd.ExpiryAlertThresholdDays,
			ProductCategory:          cmd.ProductCategory,
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
