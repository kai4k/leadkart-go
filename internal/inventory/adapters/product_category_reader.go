package adapters

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/refdata"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
)

// gstDefaultLister is the part of the shared reference-data reader this
// adapter needs ([refdata.GstDefaultReaderPG] satisfies it).
type gstDefaultLister interface {
	List(ctx context.Context) ([]refdata.CategoryGstDefault, error)
}

// ProductCategoryReader is the [query.ProductCategoryReader] over the shared
// reference table shared.product_category_gst_defaults. The SQL stays with the
// table's owner (internal/common/refdata); this adapter only maps the rows.
type ProductCategoryReader struct {
	defaults gstDefaultLister
}

// NewProductCategoryReader wires the reader.
func NewProductCategoryReader(defaults gstDefaultLister) *ProductCategoryReader {
	return &ProductCategoryReader{defaults: defaults}
}

var _ query.ProductCategoryReader = (*ProductCategoryReader)(nil)

// ListProductCategories satisfies [query.ProductCategoryReader].
func (r *ProductCategoryReader) ListProductCategories(ctx context.Context) ([]query.ProductCategoryView, error) {
	rows, err := r.defaults.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]query.ProductCategoryView, 0, len(rows))
	for _, row := range rows {
		out = append(out, query.ProductCategoryView{Category: row.Category, DefaultGSTRateBps: row.RateBps})
	}
	return out, nil
}
