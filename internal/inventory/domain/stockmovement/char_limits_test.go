package stockmovement_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// M2: a movement's reason (500) and source reference (200) are limited in
// CHARACTERS (CHECK on length()); the domain used to count bytes, so a Hindi
// reason of 167 letters was refused. A Devanagari value AT the limit is
// accepted, one character more is refused.
func TestMovement_LimitsCountCharactersNotBytes(t *testing.T) {
	t.Parallel()
	dev := func(n int) string { return strings.Repeat("क", n) }
	for field, c := range map[string]struct {
		max  int
		edit func(*stockmovement.Spec, string)
	}{
		"reason":           {stockmovement.ReasonMaxLength, func(s *stockmovement.Spec, v string) { s.Reason = v }},
		"source_reference": {stockmovement.SourceReferenceMax, func(s *stockmovement.Spec, v string) { s.SourceReference = &v }},
	} {
		mid, bid, pid, tid, actor := freshIDs(t)
		spec := stockmovement.Spec{
			BatchID: bid, ProductID: pid, TenantID: tid, Type: batch.MovementInbound,
			Quantity: 100, QuantityOnHandAfter: 100, Reason: "Initial stock-in", ActorMembershipID: actor,
		}
		c.edit(&spec, dev(c.max))
		if _, err := stockmovement.New(mid, spec, fixedNow); err != nil {
			t.Errorf("%s: %d Devanagari characters (%d bytes) must be accepted: %v", field, c.max, 3*c.max, err)
		}
		c.edit(&spec, dev(c.max+1))
		_, err := stockmovement.New(mid, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, stockmovement.ErrInvalid) || !ok || te.Field != field || te.Unit != textlimit.UnitChars || te.Max != c.max {
			t.Errorf("%s: %d characters must be refused as too long in characters, got %v", field, c.max+1, err)
		}
	}
}
