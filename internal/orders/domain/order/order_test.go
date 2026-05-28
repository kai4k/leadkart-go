package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleItems() []quotation.LineItem {
	return []quotation.LineItem{
		{
			ProductID:     ids.NewV7().String(),
			SKU:           "AMOX-500-T10",
			Description:   "Amoxicillin 500mg Tablet",
			Quantity:      100,
			UnitMrpPaise:  9500,
			UnitSalePaise: 6500,
			GstRateBps:    1200,
		},
		{
			ProductID:     ids.NewV7().String(),
			SKU:           "PARA-500-T15",
			Description:   "Paracetamol 500mg Tablet",
			Quantity:      50,
			UnitMrpPaise:  4500,
			UnitSalePaise: 3000,
			GstRateBps:    1200,
		},
	}
}

func sampleNewInput(t *testing.T) order.NewInput {
	t.Helper()
	return order.NewInput{
		ID:                    order.ID(ids.NewV7().String()),
		TenantID:              tenant.ID(ids.NewV7().String()),
		ApprovedQuotationID:   quotation.ID(ids.NewV7().String()),
		CustomerLeadID:        quotation.CustomerLeadID(ids.NewV7().String()),
		ConfirmedItems:        sampleItems(),
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
		Now:                   fixedNow(),
	}
}

func TestOrder_New_ComputesTotals(t *testing.T) {
	t.Parallel()
	in := sampleNewInput(t)
	o, err := order.New(in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 100 * 6500 = 650000 + 50 * 3000 = 150000 → subtotal 800000
	// tax = 650000 * 0.12 + 150000 * 0.12 = 78000 + 18000 = 96000
	// grand = 896000
	const wantSubtotal int64 = 800000
	const wantTax int64 = 96000
	const wantGrand int64 = 896000
	if o.SubtotalPaise() != wantSubtotal {
		t.Errorf("subtotal=%d want %d", o.SubtotalPaise(), wantSubtotal)
	}
	if o.TaxPaise() != wantTax {
		t.Errorf("tax=%d want %d", o.TaxPaise(), wantTax)
	}
	if o.GrandTotalPaise() != wantGrand {
		t.Errorf("grand=%d want %d", o.GrandTotalPaise(), wantGrand)
	}
	if o.State() != order.StateQuotationApproved {
		t.Errorf("state=%s want quotation_approved", o.State())
	}
	if got := len(o.PullEvents()); got != 1 {
		t.Errorf("events=%d want 1 (CreatedEvent)", got)
	}
}

func TestOrder_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)
	cases := []struct {
		name string
		mod  func(*order.NewInput)
	}{
		{"zero id", func(in *order.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *order.NewInput) { in.TenantID = "" }},
		{"zero quotation", func(in *order.NewInput) { in.ApprovedQuotationID = "" }},
		{"zero lead", func(in *order.NewInput) { in.CustomerLeadID = "" }},
		{"empty items", func(in *order.NewInput) { in.ConfirmedItems = nil }},
		{"zero creator", func(in *order.NewInput) { in.CreatedByMembershipID = "" }},
		{"zero now", func(in *order.NewInput) { in.Now = time.Time{} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := order.New(in); !errors.Is(err, order.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestOrder_HappyPath_FullLifecycle(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	o.PullEvents()
	actor := membership.ID(ids.NewV7().String())
	now := fixedNow()

	steps := []struct {
		name string
		fn   func() error
		want order.State
	}{
		{"token-paid", func() error { return o.RecordTokenPayment(actor, now.Add(1*time.Hour)) }, order.StateTokenPaid},
		{"confirmed", func() error { return o.Confirm(actor, now.Add(2*time.Hour)) }, order.StateConfirmed},
		{"packed", func() error { return o.MarkPacked(actor, now.Add(3*time.Hour)) }, order.StatePacked},
		{"invoiced", func() error { return o.AttachInvoice("inv-uuid-1", actor, now.Add(4*time.Hour)) }, order.StateInvoiced},
		{"dispatched", func() error { return o.AttachConsignment("cn-uuid-1", actor, now.Add(5*time.Hour)) }, order.StateDispatched},
		{"delivered", func() error { return o.MarkDelivered(actor, now.Add(6*time.Hour)) }, order.StateDelivered},
		{"complete", func() error { return o.MarkComplete(actor, now.Add(7*time.Hour)) }, order.StateComplete},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if o.State() != s.want {
			t.Fatalf("after %s state=%s want %s", s.name, o.State(), s.want)
		}
		if got := len(o.PullEvents()); got != 1 {
			t.Errorf("after %s events=%d want 1 (AdvancedEvent)", s.name, got)
		}
	}

	if o.InvoiceID() != "inv-uuid-1" {
		t.Errorf("InvoiceID=%q want inv-uuid-1", o.InvoiceID())
	}
	if o.ConsignmentNoteID() != "cn-uuid-1" {
		t.Errorf("ConsignmentNoteID=%q want cn-uuid-1", o.ConsignmentNoteID())
	}
	if o.ConfirmedAt() == nil || o.CompletedAt() == nil {
		t.Errorf("timestamps unset: confirmedAt=%v completedAt=%v", o.ConfirmedAt(), o.CompletedAt())
	}
}

func TestOrder_RejectsSkippingStates(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())

	// quotation_approved → packed is a skip; reject.
	err := o.MarkPacked(actor, fixedNow().Add(time.Hour))
	if !errors.Is(err, order.ErrInvalidTransition) {
		t.Errorf("skip MarkPacked: got %v want ErrInvalidTransition", err)
	}

	// quotation_approved → confirmed is also a skip (must pass token_paid).
	err = o.Confirm(actor, fixedNow().Add(time.Hour))
	if !errors.Is(err, order.ErrInvalidTransition) {
		t.Errorf("skip Confirm: got %v want ErrInvalidTransition", err)
	}
}

func TestOrder_SelfTransitionIsIdempotent(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	o.PullEvents()
	actor := membership.ID(ids.NewV7().String())
	require.NoError(t, o.RecordTokenPayment(actor, fixedNow().Add(time.Hour)))
	o.PullEvents()

	// Re-call RecordTokenPayment — no error, no event.
	if err := o.RecordTokenPayment(actor, fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("re-RecordTokenPayment: %v", err)
	}
	if got := len(o.PullEvents()); got != 0 {
		t.Errorf("re-RecordTokenPayment events=%d want 0", got)
	}
}

func TestOrder_Cancel_FromVariousStates(t *testing.T) {
	t.Parallel()
	actor := membership.ID(ids.NewV7().String())

	// From quotation_approved.
	{
		o, _ := order.New(sampleNewInput(t))
		o.PullEvents()
		if err := o.Cancel("customer changed mind", actor, fixedNow().Add(time.Hour)); err != nil {
			t.Fatalf("cancel from quotation_approved: %v", err)
		}
		if o.State() != order.StateCancelled {
			t.Errorf("state=%s want cancelled", o.State())
		}
		events := o.PullEvents()
		if len(events) != 1 {
			t.Fatalf("events=%d want 1", len(events))
		}
		ce, ok := events[0].(order.CancelledEvent)
		if !ok {
			t.Fatalf("event type=%T want CancelledEvent", events[0])
		}
		if ce.PriorState != order.StateQuotationApproved {
			t.Errorf("PriorState=%s want quotation_approved", ce.PriorState)
		}
	}

	// From invoiced — most interesting because compensation needs to mint a CreditNote.
	{
		o, _ := order.New(sampleNewInput(t))
		require.NoError(t, o.RecordTokenPayment(actor, fixedNow().Add(time.Hour)))
		require.NoError(t, o.Confirm(actor, fixedNow().Add(2*time.Hour)))
		require.NoError(t, o.MarkPacked(actor, fixedNow().Add(3*time.Hour)))
		require.NoError(t, o.AttachInvoice("inv-1", actor, fixedNow().Add(4*time.Hour)))
		o.PullEvents()

		if err := o.Cancel("warehouse damage", actor, fixedNow().Add(5*time.Hour)); err != nil {
			t.Fatalf("cancel from invoiced: %v", err)
		}
		events := o.PullEvents()
		if len(events) != 1 {
			t.Fatalf("events=%d want 1", len(events))
		}
		ce := events[0].(order.CancelledEvent)
		if ce.PriorState != order.StateInvoiced {
			t.Errorf("PriorState=%s want invoiced (drives CreditNote compensation per ADR 0063 §4)", ce.PriorState)
		}
	}
}

func TestOrder_CancelFromCompleteIsRejected(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())

	// Walk to complete.
	require.NoError(t, o.RecordTokenPayment(actor, fixedNow().Add(time.Hour)))
	require.NoError(t, o.Confirm(actor, fixedNow().Add(2*time.Hour)))
	require.NoError(t, o.MarkPacked(actor, fixedNow().Add(3*time.Hour)))
	require.NoError(t, o.AttachInvoice("inv-1", actor, fixedNow().Add(4*time.Hour)))
	require.NoError(t, o.AttachConsignment("cn-1", actor, fixedNow().Add(5*time.Hour)))
	require.NoError(t, o.MarkDelivered(actor, fixedNow().Add(6*time.Hour)))
	require.NoError(t, o.MarkComplete(actor, fixedNow().Add(7*time.Hour)))

	if err := o.Cancel("too late", actor, fixedNow().Add(8*time.Hour)); !errors.Is(err, order.ErrInvalidTransition) {
		t.Errorf("cancel from complete: got %v want ErrInvalidTransition", err)
	}
}

func TestOrder_CancelIdempotent(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())
	require.NoError(t, o.Cancel("first", actor, fixedNow().Add(time.Hour)))
	o.PullEvents()

	if err := o.Cancel("second", actor, fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if got := len(o.PullEvents()); got != 0 {
		t.Errorf("re-Cancel events=%d want 0", got)
	}
	if o.CancellationReason() != "first" {
		t.Errorf("reason=%q want \"first\" (idempotent must NOT overwrite)", o.CancellationReason())
	}
}

func TestOrder_AttachInvoiceRequiresInvoiceID(t *testing.T) {
	t.Parallel()
	o, _ := order.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())
	require.NoError(t, o.RecordTokenPayment(actor, fixedNow().Add(time.Hour)))
	require.NoError(t, o.Confirm(actor, fixedNow().Add(2*time.Hour)))
	require.NoError(t, o.MarkPacked(actor, fixedNow().Add(3*time.Hour)))

	if err := o.AttachInvoice("", actor, fixedNow().Add(4*time.Hour)); !errors.Is(err, order.ErrInvalid) {
		t.Errorf("AttachInvoice empty: got %v want ErrInvalid", err)
	}
}

func TestParseState(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"quotation_draft", "complete", "cancelled"} {
		if _, err := order.ParseState(ok); err != nil {
			t.Errorf("ParseState(%q) err=%v", ok, err)
		}
	}
	if _, err := order.ParseState("nonsense"); !errors.Is(err, order.ErrInvalid) {
		t.Errorf("ParseState bad: got %v want ErrInvalid", err)
	}
}

func TestOrder_UnmarshalFromDB_DoesNotEmit(t *testing.T) {
	t.Parallel()
	original, _ := order.New(sampleNewInput(t))
	original.PullEvents()

	snap := order.Snapshot{
		ID:                    original.ID(),
		TenantID:              original.TenantID(),
		ApprovedQuotationID:   original.ApprovedQuotationID(),
		CustomerLeadID:        original.CustomerLeadID(),
		State:                 order.StateConfirmed,
		ConfirmedItems:        original.ConfirmedItems(),
		SubtotalPaise:         original.SubtotalPaise(),
		TaxPaise:              original.TaxPaise(),
		GrandTotalPaise:       original.GrandTotalPaise(),
		CreatedAt:             original.CreatedAt(),
		CreatedByMembershipID: original.CreatedByMembershipID(),
	}
	hydrated := order.UnmarshalFromDB(snap)
	if hydrated.State() != order.StateConfirmed {
		t.Errorf("state=%s want confirmed", hydrated.State())
	}
	if got := len(hydrated.PullEvents()); got != 0 {
		t.Errorf("events=%d want 0 (rehydration must not emit)", got)
	}
}
