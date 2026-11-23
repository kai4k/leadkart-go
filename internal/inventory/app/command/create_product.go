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
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// CreateProductCommand carries the validated input for creating a new
// Product. ActorMembershipID populates the CreatedByMembershipID on the
// integration event for audit.
//
// GSTRateID is the entry of the company GST rate list the product uses; it is
// REQUIRED (there is no default rate and no category fallback) and its rate is
// the list entry's rate. DrugSchedule is a class id from the company own list:
// nullable unless the company chose `required`. ReorderLevel and ProductCategory
// carry their canonical defaults (0 and "General") when omitted.
// ExpiryAlertThresholdDays: nil means "not given" and takes
// [product.ExpiryAlertThresholdDaysDefault] (90, BRD section 6.5); a non-nil value,
// including an explicit 0 (the alert switched off), is used as given.
type CreateProductCommand struct {
	TenantID                 tenant.ID
	ActorMembershipID        membership.ID
	SKU                      string
	Name                     string
	DosageForm               string
	PackSize                 string
	HSNCode                  string
	GSTRateID                string
	Manufacturer             string
	ReorderLevel             int
	ExpiryAlertThresholdDays *int
	ProductCategory          string
	DefaultSaleRatePaise     int64
	GenericName              string
	Composition              string
	StorageCondition         string
	DrugSchedule             string
	// DrugScheduleGiven says whether the request named drug_schedule at all.
	DrugScheduleGiven bool
}

// CreateProductResult carries the new ProductID.
type CreateProductResult struct {
	ProductID product.ID
}

// CreateProductHandler — single-aggregate insert + outbox drain.
// Wire-contract: ErrSKUTaken surfaces as HTTP 409.
//
// The company lists (GST rates, drug schedule classes) are read from the
// Inventory replica of the policy.
type CreateProductHandler struct {
	products     product.Repository
	policy       sellability.PolicyReader
	now          func() time.Time
	newProductID func() product.ID
}

// NewCreateProductHandler wires the handler. `now` is the explicit time
// source — composition root wires `time.Now`; tests inject a fixed-time
// closure. Nil → time.Now.
//
// newProductID is the aggregate-ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline.
func NewCreateProductHandler(products product.Repository, policy sellability.PolicyReader, now func() time.Time, newProductID func() product.ID) CreateProductHandler {
	if newProductID == nil || policy == nil {
		panic("command: NewCreateProductHandler newProductID and policy required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateProductHandler{products: products, policy: policy, now: now, newProductID: newProductID}
}

// Handle constructs + persists the Product. Outbox event drain rides
// the adapter's Add path (one-tx with the row insert per ADR 0008).
//
// The GST rate and the drug schedule class are checked against the company lists;
// a refusal is a [*FieldRefusal] (422 with errors{field:[code]}) and nothing is written.
func (h CreateProductHandler) Handle(ctx context.Context, cmd CreateProductCommand) (CreateProductResult, error) {
	pol, err := h.policy.ReadPolicy(ctx, cmd.TenantID)
	if err != nil {
		return CreateProductResult{}, fmt.Errorf("create product: read policy: %w", err)
	}
	gst, refusal := resolveCatalogue(pol.Policy, catalogueChoice{
		GSTRateID: cmd.GSTRateID, RequireGST: true, DrugSchedule: cmd.DrugSchedule, ScheduleGiven: cmd.DrugScheduleGiven, Create: true,
	})
	if refusal != nil {
		return CreateProductResult{}, refusal
	}
	threshold := product.ExpiryAlertThresholdDaysDefault
	if cmd.ExpiryAlertThresholdDays != nil {
		threshold = *cmd.ExpiryAlertThresholdDays
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
			ExpiryAlertThresholdDays: threshold,
			ProductCategory:          cmd.ProductCategory,
			GSTRateID:                cmd.GSTRateID,
			DefaultSaleRatePaise:     cmd.DefaultSaleRatePaise,
			GenericName:              cmd.GenericName,
			Composition:              cmd.Composition,
			StorageCondition:         cmd.StorageCondition,
			DrugScheduleClassID:      cmd.DrugSchedule,
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
