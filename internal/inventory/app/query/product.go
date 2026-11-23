// Package query holds Inventory CQRS query handlers — pure read-side,
// no state mutation. Mirror of the command package shape.
package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ProductView is the flat read model for a single product: the catalogue row
// plus the stock figures a screen shows next to it. Per STRICT CQRS (TDL
// canon) the [product.Product] aggregate NEVER leaks past the app layer into
// ports/. The port serializes this View into the wire ProductDto (1:1).
//
// The stock figures cross two aggregates (Product and its Batches), so they
// are read by a [ProductReader] in one statement, not assembled from
// repositories.
type ProductView struct {
	ID           string
	TenantID     string
	SKU          string
	Name         string
	DosageForm   string
	PackSize     string
	HSNCode      string
	GSTRateBps   int
	Manufacturer string
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time

	ProductCategory          string
	ReorderLevel             int
	ExpiryAlertThresholdDays int

	// The G2 catalogue fields. GSTRateLabel and DrugScheduleLabel are resolved from
	// the company policy (the lists the ids point into) by the query handlers; they
	// are empty when the id is empty or no longer in the list.
	GSTRateID            string
	GSTRateLabel         string
	DefaultSaleRatePaise int64
	GenericName          string
	Composition          string
	StorageCondition     string
	DrugScheduleClassID  string
	DrugScheduleLabel    string

	// The five stock buckets of the live batches; they sum: StockOnHand =
	// StockReserved + StockAvailable + StockUnsellable + StockQuarantined.
	// Available is the free units of sellable batches (what can be sold);
	// Unsellable the free units past the company cut-off (not quarantined yet);
	// Quarantined the units set apart as "Not for sale".
	StockOnHand      int64
	StockReserved    int64
	StockAvailable   int64
	StockUnsellable  int64
	StockQuarantined int64
	// EarliestExpiry is the soonest expiry among SELLABLE batches that hold
	// stock; nil when the product has none.
	EarliestExpiry *time.Time
}

// ProductListFilter narrows the product list. The zero value filters nothing.
type ProductListFilter struct {
	// Search is a case-insensitive substring of "sku name".
	Search string
	// ActiveOnly keeps is_active products only.
	ActiveOnly bool
	// Category, DosageForm and Manufacturer are exact, case-insensitive matches.
	Category     string
	DosageForm   string
	Manufacturer string
	// LowStock keeps products with a reorder level above 0 whose StockOnHand
	// is below it (the rule the daily reorder scan alerts on).
	LowStock bool
	// ExpiringWithinDays, when set, keeps products whose EarliestExpiry is
	// on or before today plus that many days.
	ExpiringWithinDays *int
}

// ProductReader is the read side of the product screens, declared with its
// consumer. The implementation lives in inventory/adapters.
//
// today is the date "expired" is judged against (UTC, date only), passed in
// so the handler and the SQL share one moment. tenantID is explicit per
// ADR 0062; RLS remains the backstop.
type ProductReader interface {
	// GetProduct returns the live product or [product.ErrNotFound].
	GetProduct(ctx context.Context, tenantID tenant.ID, id product.ID, w sellability.Window) (ProductView, error)
	// ListProducts returns at most limit live products after cursor, ordered
	// (created_at DESC, id DESC) per ADR 0038.
	ListProducts(ctx context.Context, tenantID tenant.ID, filter ProductListFilter, w sellability.Window, cursor pagination.Cursor, limit int) ([]ProductView, error)
}

// GetProductQuery — single-product read.
//
// TenantID is the caller's tenant scope (injected from JWT context by
// the HTTP layer). Per ADR 0062 (TDL canon): tenantID flows through
// explicit query fields, not via context smuggling.
type GetProductQuery struct {
	TenantID  tenant.ID
	ProductID product.ID
}

// GetProductHandler returns a Product view or product.ErrNotFound.
type GetProductHandler struct {
	products ProductReader
	window   sellability.Resolver
}

// NewGetProductHandler wires the handler. now is the explicit time source
// (nil → time.Now); it decides which batches are expired.
func NewGetProductHandler(products ProductReader, window sellability.Resolver) GetProductHandler {
	return GetProductHandler{products: products, window: window}
}

// Handle returns the product View or product.ErrNotFound.
func (h GetProductHandler) Handle(ctx context.Context, q GetProductQuery) (ProductView, error) {
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return ProductView{}, fmt.Errorf("get product: %w", err)
	}
	v, err := h.products.GetProduct(ctx, q.TenantID, q.ProductID, w)
	if err != nil {
		return ProductView{}, err
	}
	return labelled(v, w), nil
}

// ListProductsPageQuery — cursor-paginated product list under tenant scope.
type ListProductsPageQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
	Filter   ProductListFilter
}

// ListProductsPageHandler returns a keyset-paginated page.
type ListProductsPageHandler struct {
	products ProductReader
	window   sellability.Resolver
}

// NewListProductsPageHandler wires the handler. now is the explicit time
// source (nil → time.Now); it decides which batches are expired.
func NewListProductsPageHandler(products ProductReader, window sellability.Resolver) ListProductsPageHandler {
	return ListProductsPageHandler{products: products, window: window}
}

// Handle returns the page of ProductView. It asks the reader for one row more
// than the page holds; pagination.BuildPage turns the extra row into HasMore.
func (h ListProductsPageHandler) Handle(ctx context.Context, q ListProductsPageQuery) (pagination.Page[ProductView], error) {
	size := pagination.ClampPageSize(q.PageSize)
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return pagination.Page[ProductView]{}, fmt.Errorf("list products: %w", err)
	}
	rows, err := h.products.ListProducts(ctx, q.TenantID, q.Filter, w, q.Cursor, size+1)
	if err != nil {
		return pagination.Page[ProductView]{}, fmt.Errorf("list products: %w", err)
	}
	for i := range rows {
		rows[i] = labelled(rows[i], w)
	}
	page := pagination.BuildPage(rows, size, func(v ProductView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.CreatedAt, ID: v.ID}
	})
	if page.Items == nil {
		page.Items = []ProductView{}
	}
	return page, nil
}

// labelled fills the labels of the company lists the product ids point into.
func labelled(v ProductView, w sellability.Window) ProductView {
	for _, r := range w.Policy.Tax.GSTRates {
		if r.ID == v.GSTRateID {
			v.GSTRateLabel = r.Label
		}
	}
	if c, ok := w.Policy.DrugSchedule.ScheduleClassByID(v.DrugScheduleClassID); ok {
		v.DrugScheduleLabel = c.Label
	}
	return v
}
