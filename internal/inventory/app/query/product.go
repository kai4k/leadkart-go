// Package query holds Inventory CQRS query handlers — pure read-side,
// no state mutation. Mirror of the command package shape.
package query

import (
	"context"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// GetProductQuery — single-product read.
type GetProductQuery struct {
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

// Handle returns the product.
func (h GetProductHandler) Handle(ctx context.Context, q GetProductQuery) (*product.Product, error) {
	return h.products.GetByID(ctx, q.ProductID)
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

// Handle returns the page.
func (h ListProductsPageHandler) Handle(ctx context.Context, q ListProductsPageQuery) (pagination.Page[*product.Product], error) {
	page, err := h.products.ListPage(ctx, q.TenantID, product.ListFilter{
		Search:       q.Search,
		ActiveOnly:   q.ActiveOnly,
		DosageForm:   q.DosageForm,
		Manufacturer: q.Manufacturer,
	}, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[*product.Product]{}, fmt.Errorf("list products: %w", err)
	}
	return page, nil
}
