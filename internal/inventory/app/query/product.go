// Package query holds Inventory CQRS query handlers — pure read-side,
// no state mutation. Mirror of the command package shape.
package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// ProductView is the flat read model for a single product. Per STRICT
// CQRS (TDL canon) query handlers project the write aggregate into this
// read DTO; the [product.Product] aggregate NEVER leaks past the app
// layer into ports/. The port serializes this View into the wire
// ProductDto (1:1).
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
}

// projectProduct maps the write aggregate to the flat read View — the
// single source of truth for product read projection.
func projectProduct(p *product.Product) ProductView {
	return ProductView{
		ID:           p.ID().String(),
		TenantID:     p.TenantID().String(),
		SKU:          p.SKU(),
		Name:         p.Name(),
		DosageForm:   p.DosageForm(),
		PackSize:     p.PackSize(),
		HSNCode:      p.HSNCode(),
		GSTRateBps:   p.GSTRateBps(),
		Manufacturer: p.Manufacturer(),
		IsActive:     p.IsActive(),
		CreatedAt:    p.CreatedAt(),
		UpdatedAt:    p.UpdatedAt(),
	}
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

// GetProductHandler returns a Product or product.ErrNotFound.
type GetProductHandler struct {
	products product.Repository
}

// NewGetProductHandler wires the handler.
func NewGetProductHandler(products product.Repository) GetProductHandler {
	return GetProductHandler{products: products}
}

// Handle returns the product View or product.ErrNotFound.
func (h GetProductHandler) Handle(ctx context.Context, q GetProductQuery) (ProductView, error) {
	p, err := h.products.GetByID(ctx, q.TenantID, q.ProductID)
	if err != nil {
		return ProductView{}, err
	}
	return projectProduct(p), nil
}

// ListProductsPageQuery — cursor-paginated product list under tenant
// scope. Filters: ActiveOnly / DosageForm / Manufacturer / Search.
type ListProductsPageQuery struct {
	TenantID     tenant.ID
	Cursor       pagination.Cursor
	PageSize     int
	ActiveOnly   bool
	DosageForm   string
	Manufacturer string
	Search       string
}

// ListProductsPageHandler returns a keyset-paginated page.
type ListProductsPageHandler struct {
	products product.Repository
}

// NewListProductsPageHandler wires the handler.
func NewListProductsPageHandler(products product.Repository) ListProductsPageHandler {
	return ListProductsPageHandler{products: products}
}

// Handle returns the page of ProductView.
func (h ListProductsPageHandler) Handle(ctx context.Context, q ListProductsPageQuery) (pagination.Page[ProductView], error) {
	page, err := h.products.ListPage(ctx, q.TenantID, product.ListFilter{
		Search:       q.Search,
		ActiveOnly:   q.ActiveOnly,
		DosageForm:   q.DosageForm,
		Manufacturer: q.Manufacturer,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[ProductView]{}, fmt.Errorf("list products: %w", err)
	}
	views := make([]ProductView, 0, len(page.Items))
	for _, p := range page.Items {
		views = append(views, projectProduct(p))
	}
	return pagination.Page[ProductView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}
