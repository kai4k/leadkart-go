package batch_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
)

// M2: the batch columns are limited in CHARACTERS (CHECK on length()); the
// domain used to count bytes and refuse valid Hindi. A Devanagari value AT the
// limit is accepted, one character more is refused.
func TestBatch_LimitsCountCharactersNotBytes(t *testing.T) {
	t.Parallel()
	dev := func(n int) string { return strings.Repeat("क", n) }
	for field, c := range map[string]struct {
		max  int
		edit func(*batch.Spec, string)
	}{
		"batch_number":                 {batch.BatchNumberMaxLen, func(s *batch.Spec, v string) { s.BatchNumber = v }},
		"manufacturer_name":            {batch.ManufacturerNameMaxLen, func(s *batch.Spec, v string) { s.ManufacturerName = v }},
		"manufacturing_licence_number": {batch.ManufacturingLicenceMax, func(s *batch.Spec, v string) { s.ManufacturingLicenceNumber = v }},
	} {
		bid, pid, tid, actor := freshIDs(t)
		spec := validBatchSpec()
		c.edit(&spec, dev(c.max))
		if _, err := batch.New(bid, pid, tid, actor, spec, fixedNow); err != nil {
			t.Errorf("%s: %d Devanagari characters (%d bytes) must be accepted: %v", field, c.max, 3*c.max, err)
		}
		c.edit(&spec, dev(c.max+1))
		_, err := batch.New(bid, pid, tid, actor, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, batch.ErrInvalid) || !ok || te.Field != field || te.Unit != textlimit.UnitChars || te.Max != c.max {
			t.Errorf("%s: %d characters must be refused as too long in characters, got %v", field, c.max+1, err)
		}
	}
}
