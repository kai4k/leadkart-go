package quotation_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

func fixedNow() time.Time {
	return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
}

func sampleItem() quotation.LineItem {
	return quotation.LineItem{
		ProductID:     ids.NewV7().String(),
		SKU:           "AMOX-500-T10",
		Description:   "Amoxicillin 500mg Tablet (10s)",
		Quantity:      100,
		UnitMrpPaise:  9500,
		UnitSalePaise: 6500,
		GstRateBps:    1200,
	}
}

func sampleNewInput(t *testing.T) quotation.NewInput {
	t.Helper()
	return quotation.NewInput{
		ID:                    quotation.ID(ids.NewV7().String()),
		TenantID:              tenant.ID(ids.NewV7().String()),
		CustomerLeadID:        quotation.CustomerLeadID(ids.NewV7().String()),
		InitialItems:          []quotation.LineItem{sampleItem()},
		InitialNote:           "First quote draft",
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
		Now:                   fixedNow(),
	}
}

func TestQuotation_New_HappyPath(t *testing.T) {
	t.Parallel()
	in := sampleNewInput(t)
	q, err := quotation.New(in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if q.ID() != in.ID {
		t.Errorf("ID mismatch: got %s want %s", q.ID(), in.ID)
	}
	if q.State() != quotation.StateDraft {
		t.Errorf("State=%s want draft", q.State())
	}
	if got := len(q.Revisions()); got != 1 {
		t.Errorf("revisions len=%d want 1", got)
	}
	rev := q.CurrentRevision()
	if rev.Number != 1 {
		t.Errorf("rev number=%d want 1", rev.Number)
	}
	if len(rev.Items) != 1 {
		t.Errorf("rev items len=%d want 1", len(rev.Items))
	}

	events := q.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events len=%d want 1", len(events))
	}
	if _, ok := events[0].(quotation.CreatedEvent); !ok {
		t.Errorf("event 0 type=%T want CreatedEvent", events[0])
	}
}

func TestQuotation_New_RejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)

	cases := []struct {
		name string
		mod  func(*quotation.NewInput)
	}{
		{"zero id", func(in *quotation.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *quotation.NewInput) { in.TenantID = "" }},
		{"zero customer lead", func(in *quotation.NewInput) { in.CustomerLeadID = "" }},
		{"zero creator", func(in *quotation.NewInput) { in.CreatedByMembershipID = "" }},
		{"zero now", func(in *quotation.NewInput) { in.Now = time.Time{} }},
		{"empty items", func(in *quotation.NewInput) { in.InitialItems = nil }},
		{"item product_id missing", func(in *quotation.NewInput) {
			it := sampleItem()
			it.ProductID = ""
			in.InitialItems = []quotation.LineItem{it}
		}},
		{"sale > mrp", func(in *quotation.NewInput) {
			it := sampleItem()
			it.UnitMrpPaise = 5000
			it.UnitSalePaise = 6000
			in.InitialItems = []quotation.LineItem{it}
		}},
		{"gst rate out of range", func(in *quotation.NewInput) {
			it := sampleItem()
			it.GstRateBps = 10001
			in.InitialItems = []quotation.LineItem{it}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := quotation.New(in); err == nil {
				t.Fatal("want error")
			} else if !errors.Is(err, quotation.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestQuotation_Revise_AppendsRevisionAndEmitsEvent(t *testing.T) {
	t.Parallel()
	q, _ := quotation.New(sampleNewInput(t))
	q.PullEvents() // discard CreatedEvent

	revInput := quotation.ReviseInput{
		Items:               []quotation.LineItem{sampleItem(), sampleItem()},
		Note:                "Added second line",
		RevisedByMembership: membership.ID(ids.NewV7().String()),
		Now:                 fixedNow().Add(time.Hour),
	}
	if err := q.Revise(revInput); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if got := len(q.Revisions()); got != 2 {
		t.Errorf("revisions len=%d want 2", got)
	}
	if q.CurrentRevision().Number != 2 {
		t.Errorf("current revision number=%d want 2", q.CurrentRevision().Number)
	}
	if len(q.CurrentRevision().Items) != 2 {
		t.Errorf("current revision items=%d want 2", len(q.CurrentRevision().Items))
	}
	events := q.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events len=%d want 1", len(events))
	}
	if _, ok := events[0].(quotation.RevisedEvent); !ok {
		t.Errorf("event 0 type=%T want RevisedEvent", events[0])
	}
}

func TestQuotation_Approve_FreezesTipAndIsIdempotent(t *testing.T) {
	t.Parallel()
	q, _ := quotation.New(sampleNewInput(t))
	q.PullEvents()

	approver := membership.ID(ids.NewV7().String())
	if err := q.Approve(approver, fixedNow().Add(time.Hour)); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if q.State() != quotation.StateApproved {
		t.Errorf("state=%s want approved", q.State())
	}
	if q.ApprovedByMembershipID() == nil || *q.ApprovedByMembershipID() != approver {
		t.Errorf("ApprovedByMembershipID mismatch")
	}
	events := q.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events len=%d want 1", len(events))
	}

	// Idempotent re-approve: no event, no error.
	if err := q.Approve(approver, fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("re-Approve: %v", err)
	}
	if got := len(q.PullEvents()); got != 0 {
		t.Errorf("re-Approve events=%d want 0 (idempotent)", got)
	}
}

func TestQuotation_Reject_TerminalAndIdempotent(t *testing.T) {
	t.Parallel()
	q, _ := quotation.New(sampleNewInput(t))
	q.PullEvents()

	rejector := membership.ID(ids.NewV7().String())
	if err := q.Reject(rejector, "customer chose competitor", fixedNow().Add(time.Hour)); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if q.State() != quotation.StateRejected {
		t.Errorf("state=%s want rejected", q.State())
	}
	if q.RejectionReason() != "customer chose competitor" {
		t.Errorf("reason mismatch: %q", q.RejectionReason())
	}
	q.PullEvents()

	// Idempotent re-reject: no event, no error.
	if err := q.Reject(rejector, "customer chose competitor", fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("re-Reject: %v", err)
	}
	if got := len(q.PullEvents()); got != 0 {
		t.Errorf("re-Reject events=%d want 0", got)
	}
}

func TestQuotation_TerminalGuards(t *testing.T) {
	t.Parallel()

	// Approved → cannot revise, cannot reject.
	{
		q, _ := quotation.New(sampleNewInput(t))
		require.NoError(t, q.Approve(membership.ID(ids.NewV7().String()), fixedNow()))
		q.PullEvents()

		err := q.Revise(quotation.ReviseInput{
			Items:               []quotation.LineItem{sampleItem()},
			RevisedByMembership: membership.ID(ids.NewV7().String()),
			Now:                 fixedNow().Add(time.Hour),
		})
		if !errors.Is(err, quotation.ErrInvalidTransition) {
			t.Errorf("Revise after Approve: got %v want ErrInvalidTransition", err)
		}

		err = q.Reject(membership.ID(ids.NewV7().String()), "x", fixedNow().Add(time.Hour))
		if !errors.Is(err, quotation.ErrInvalidTransition) {
			t.Errorf("Reject after Approve: got %v want ErrInvalidTransition", err)
		}
	}

	// Rejected → cannot revise, cannot approve.
	{
		q, _ := quotation.New(sampleNewInput(t))
		require.NoError(t, q.Reject(membership.ID(ids.NewV7().String()), "x", fixedNow()))
		q.PullEvents()

		err := q.Revise(quotation.ReviseInput{
			Items:               []quotation.LineItem{sampleItem()},
			RevisedByMembership: membership.ID(ids.NewV7().String()),
			Now:                 fixedNow().Add(time.Hour),
		})
		if !errors.Is(err, quotation.ErrInvalidTransition) {
			t.Errorf("Revise after Reject: got %v want ErrInvalidTransition", err)
		}

		err = q.Approve(membership.ID(ids.NewV7().String()), fixedNow().Add(time.Hour))
		if !errors.Is(err, quotation.ErrInvalidTransition) {
			t.Errorf("Approve after Reject: got %v want ErrInvalidTransition", err)
		}
	}
}

func TestQuotation_UnmarshalFromDB_Roundtrip(t *testing.T) {
	t.Parallel()
	original, _ := quotation.New(sampleNewInput(t))
	original.PullEvents()

	rev := quotation.Revision{
		Number:              2,
		Items:               []quotation.LineItem{sampleItem()},
		Note:                "rev 2",
		RevisedAt:           fixedNow().Add(time.Hour),
		RevisedByMembership: membership.ID(ids.NewV7().String()),
	}
	approvedAt := fixedNow().Add(2 * time.Hour)
	approver := membership.ID(ids.NewV7().String())

	snap := quotation.Snapshot{
		ID:                     original.ID(),
		TenantID:               original.TenantID(),
		CustomerLeadID:         original.CustomerLeadID(),
		State:                  quotation.StateApproved,
		Revisions:              append(original.Revisions(), rev),
		ApprovedAt:             &approvedAt,
		ApprovedByMembershipID: &approver,
		CreatedAt:              original.CreatedAt(),
		CreatedByMembershipID:  original.CreatedByMembershipID(),
	}
	hydrated := quotation.UnmarshalFromDB(snap)
	if hydrated.State() != quotation.StateApproved {
		t.Errorf("hydrated state=%s want approved", hydrated.State())
	}
	if len(hydrated.Revisions()) != 2 {
		t.Errorf("hydrated revisions=%d want 2", len(hydrated.Revisions()))
	}
	if got := len(hydrated.PullEvents()); got != 0 {
		t.Errorf("hydrated PullEvents=%d want 0 (re-hydration must not emit)", got)
	}
}

func TestParseState(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"draft", "approved", "rejected"} {
		if _, err := quotation.ParseState(ok); err != nil {
			t.Errorf("ParseState(%q) err=%v want nil", ok, err)
		}
	}
	if _, err := quotation.ParseState("nonsense"); !errors.Is(err, quotation.ErrInvalid) {
		t.Errorf("ParseState bad: got %v want ErrInvalid", err)
	}
}
