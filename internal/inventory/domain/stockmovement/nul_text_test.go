package stockmovement_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// M1: a movement's reason and source reference refuse U+0000 (422
// validation_failed at the route, naming the field) instead of failing the
// insert as a 500.
func TestMovement_NULInTextIsRefused(t *testing.T) {
	t.Parallel()
	nul := "bad\x00value"
	for field, edit := range map[string]func(*stockmovement.Spec){
		"reason":           func(s *stockmovement.Spec) { s.Reason = nul },
		"source_reference": func(s *stockmovement.Spec) { s.SourceReference = &nul },
	} {
		mid, bid, pid, tid, actor := freshIDs(t)
		spec := stockmovement.Spec{
			BatchID: bid, ProductID: pid, TenantID: tid, Type: batch.MovementInbound,
			Quantity: 100, QuantityOnHandAfter: 100, Reason: "Initial stock-in", ActorMembershipID: actor,
		}
		edit(&spec)
		_, err := stockmovement.New(mid, spec, fixedNow)
		te, ok := textlimit.Of(err)
		if !errors.Is(err, stockmovement.ErrInvalid) || !ok || te.Field != field || !te.NUL {
			t.Fatalf("%s: want ErrInvalid naming the field, got %v", field, err)
		}
	}
}
