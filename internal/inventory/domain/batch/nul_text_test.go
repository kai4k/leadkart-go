package batch_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
)

// M1: every text field of a batch refuses U+0000 (422 validation_failed at the
// route, naming the field) instead of failing the insert as a 500.
func TestBatch_NULInTextIsRefused(t *testing.T) {
	t.Parallel()
	const nul = "bad\x00value"
	for field, edit := range map[string]func(*batch.Spec){
		"batch_number":                 func(s *batch.Spec) { s.BatchNumber = nul },
		"manufacturer_name":            func(s *batch.Spec) { s.ManufacturerName = nul },
		"manufacturing_licence_number": func(s *batch.Spec) { s.ManufacturingLicenceNumber = nul },
	} {
		bid, pid, tid, actor := freshIDs(t)
		spec := validBatchSpec()
		edit(&spec)
		_, err := batch.New(bid, pid, tid, actor, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, batch.ErrInvalid) || !ok || te.Field != field || !te.NUL {
			t.Fatalf("%s: want ErrInvalid naming the field, got %v", field, err)
		}
	}
}
