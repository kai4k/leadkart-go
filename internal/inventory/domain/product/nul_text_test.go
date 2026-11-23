package product_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// M1: every text field of a product refuses U+0000 (a text column cannot hold
// it; the insert used to fail as a 500). The refusal is ErrInvalid, which the
// route answers as 422 validation_failed, and it names the field.
func TestProduct_NULInTextIsRefused(t *testing.T) {
	t.Parallel()
	const nul = "bad\x00value"
	for field, edit := range map[string]func(*product.Spec){
		"sku":          func(s *product.Spec) { s.SKU = nul },
		"name":         func(s *product.Spec) { s.Name = nul },
		"dosage_form":  func(s *product.Spec) { s.DosageForm = nul },
		"pack_size":    func(s *product.Spec) { s.PackSize = nul },
		"manufacturer": func(s *product.Spec) { s.Manufacturer = nul },
	} {
		pid, tid, actor := freshIDs(t)
		spec := validSpec()
		edit(&spec)
		_, err := product.New(pid, tid, actor, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, product.ErrInvalid) || !ok || te.Field != field || !te.NUL {
			t.Fatalf("%s: want ErrInvalid naming the field, got %v", field, err)
		}
	}
	for field, spec := range map[string]product.UpdateSpec{
		"name":         {Name: strPtr(nul)},
		"manufacturer": {Manufacturer: strPtr(nul)},
	} {
		p := freshProduct(t)
		_, _, actor := freshIDs(t)
		err := p.Update(actor, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, product.ErrInvalid) || !ok || te.Field != field || !te.NUL {
			t.Fatalf("update %s: want ErrInvalid naming the field, got %v", field, err)
		}
		if len(p.PullEvents()) != 0 {
			t.Fatalf("update %s: a refused update emitted an event", field)
		}
	}
}
