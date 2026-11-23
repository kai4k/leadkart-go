package product_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// dev is n Devanagari letters: n characters, 3n bytes of UTF-8.
func dev(n int) string { return strings.Repeat("क", n) }

// M2: the product columns are limited by a CHECK on length(), which counts
// CHARACTERS. The domain used to count BYTES, so a Hindi name of 67 letters (201
// bytes) was refused although the column takes 200 letters. Every field accepts
// a Devanagari value AT its limit and refuses one character more.
func TestProduct_LimitsCountCharactersNotBytes(t *testing.T) {
	t.Parallel()
	for field, c := range map[string]struct {
		max  int
		edit func(*product.Spec, string)
	}{
		"sku":              {product.SKUMaxLength, func(s *product.Spec, v string) { s.SKU = v }},
		"name":             {product.NameMaxLength, func(s *product.Spec, v string) { s.Name = v }},
		"dosage_form":      {product.DosageFormMaxLength, func(s *product.Spec, v string) { s.DosageForm = v }},
		"pack_size":        {product.PackSizeMaxLength, func(s *product.Spec, v string) { s.PackSize = v }},
		"manufacturer":     {product.ManufacturerMaxLen, func(s *product.Spec, v string) { s.Manufacturer = v }},
		"product_category": {product.ProductCategoryMaxLength, func(s *product.Spec, v string) { s.ProductCategory = v }},
	} {
		pid, tid, actor := freshIDs(t)
		spec := validSpec()
		c.edit(&spec, dev(c.max))
		if _, err := product.New(pid, tid, actor, spec, fixedNow); err != nil {
			t.Errorf("%s: %d Devanagari characters (%d bytes) must be accepted: %v", field, c.max, 3*c.max, err)
		}
		c.edit(&spec, dev(c.max+1))
		_, err := product.New(pid, tid, actor, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, product.ErrInvalid) || !ok || te.Field != field || te.Unit != textlimit.UnitChars || te.Max != c.max {
			t.Errorf("%s: %d characters must be refused as too long in characters, got %v", field, c.max+1, err)
		}
	}

	// The partial update applies the same unit.
	for field, c := range map[string]struct {
		max  int
		spec func(string) product.UpdateSpec
	}{
		"name":             {product.NameMaxLength, func(v string) product.UpdateSpec { return product.UpdateSpec{Name: &v} }},
		"manufacturer":     {product.ManufacturerMaxLen, func(v string) product.UpdateSpec { return product.UpdateSpec{Manufacturer: &v} }},
		"product_category": {product.ProductCategoryMaxLength, func(v string) product.UpdateSpec { return product.UpdateSpec{ProductCategory: &v} }},
	} {
		_, _, actor := freshIDs(t)
		if err := freshProduct(t).Update(actor, c.spec(dev(c.max)), fixedNow); err != nil {
			t.Errorf("update %s: %d Devanagari characters must be accepted: %v", field, c.max, err)
		}
		err := freshProduct(t).Update(actor, c.spec(dev(c.max+1)), fixedNow)
		if te, ok := textlimit.Of(err); !errors.Is(err, product.ErrInvalid) || !ok || te.Field != field || te.Unit != textlimit.UnitChars {
			t.Errorf("update %s: %d characters must be refused, got %v", field, c.max+1, err)
		}
	}
}
