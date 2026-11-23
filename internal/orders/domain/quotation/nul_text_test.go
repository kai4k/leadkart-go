package quotation_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// M1: a quotation's free text (note, rejection reason, an item's SKU and
// description) is stored in jsonb / text, which cannot hold U+0000. Each is
// refused with the field's code instead of failing the insert (a 500).
func TestQuotation_NULInTextIsRefusedWithTheFieldsCode(t *testing.T) {
	t.Parallel()
	const nul = "bad\x00value"
	wantCode := func(t *testing.T, err error, code string) {
		t.Helper()
		te, ok := textlimit.Of(err)
		if !errors.Is(err, quotation.ErrInvalid) || !ok || te.Code() != code {
			t.Fatalf("want ErrInvalid + %s, got %v", code, err)
		}
	}

	in := sampleNewInput(t)
	in.InitialNote = nul
	_, err := quotation.New(in)
	wantCode(t, err, "note_invalid_characters")

	for field, edit := range map[string]func(*quotation.LineItem){
		"sku":         func(li *quotation.LineItem) { li.SKU = nul },
		"description": func(li *quotation.LineItem) { li.Description = nul },
	} {
		in := sampleNewInput(t)
		edit(&in.InitialItems[0])
		_, err := quotation.New(in)
		wantCode(t, err, field+"_invalid_characters")
	}

	q, err := quotation.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor := membership.ID(ids.NewV7().String())
	err = q.Revise(quotation.ReviseInput{Items: []quotation.LineItem{sampleItem()}, Note: nul, RevisedByMembership: actor, Now: fixedNow()})
	wantCode(t, err, "note_invalid_characters")
	if got := len(q.Revisions()); got != 1 {
		t.Fatalf("a refused revise added a revision: %d", got)
	}
	err = q.Reject(actor, nul, fixedNow())
	wantCode(t, err, "reason_invalid_characters")
	if q.State() == quotation.StateRejected {
		t.Fatal("a refused reject changed the state")
	}
}
