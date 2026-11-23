package query

import (
	"context"
	"fmt"
)

// ProductCategoryView is one allowed product category. It carries no GST rate:
// a product takes its rate from the company own GST list, never from its category.
type ProductCategoryView struct {
	Category string
}

// ProductCategoryReader is the read-side interface for the category reference
// list, declared with its consumer. The data is platform-wide (BRD §8.5), not
// tenant-scoped, so the method takes no tenant.
type ProductCategoryReader interface {
	// ListProductCategories returns every category, in category-name order.
	ListProductCategories(ctx context.Context) ([]ProductCategoryView, error)
}

// ListProductCategoriesQuery has no input: the list is the same for everyone.
type ListProductCategoriesQuery struct{}

// ListProductCategoriesHandler serves the product form's category options.
type ListProductCategoriesHandler struct {
	reader ProductCategoryReader
}

// NewListProductCategoriesHandler wires the handler.
func NewListProductCategoriesHandler(reader ProductCategoryReader) ListProductCategoriesHandler {
	return ListProductCategoriesHandler{reader: reader}
}

// Handle returns the categories in category-name order.
func (h ListProductCategoriesHandler) Handle(ctx context.Context, _ ListProductCategoriesQuery) ([]ProductCategoryView, error) {
	out, err := h.reader.ListProductCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list product categories: %w", err)
	}
	return out, nil
}
