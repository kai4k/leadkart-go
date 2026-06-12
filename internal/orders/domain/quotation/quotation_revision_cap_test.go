package quotation_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// TestQuotation_Revise_CapsHistory pins MaxRevisions: the JSONB history is
// rewritten wholesale per revise, so the domain bounds it. Revision 1 is the
// creation snapshot → MaxRevisions-1 further revises succeed, the next fails.
func TestQuotation_Revise_CapsHistory(t *testing.T) {
	t.Parallel()
	q, err := quotation.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	actor := membership.ID(ids.NewV7().String())
	in := quotation.ReviseInput{
		Items:               []quotation.LineItem{sampleItem()},
		RevisedByMembership: actor,
		Now:                 fixedNow(),
	}
	for i := 0; i < quotation.MaxRevisions-1; i++ {
		if err := q.Revise(in); err != nil {
			t.Fatalf("revise %d: %v", i+2, err)
		}
	}
	if got := q.CurrentRevision().Number; got != quotation.MaxRevisions {
		t.Fatalf("revision count = %d, want %d", got, quotation.MaxRevisions)
	}
	if err := q.Revise(in); !errors.Is(err, quotation.ErrInvalid) {
		t.Fatalf("revise beyond cap: err=%v want ErrInvalid", err)
	}
}
