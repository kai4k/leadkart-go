package consignmentnote_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleNewInput(t *testing.T) consignmentnote.NewInput {
	t.Helper()
	eta := fixedNow().Add(48 * time.Hour)
	return consignmentnote.NewInput{
		ID:                    consignmentnote.ID(ids.NewV7().String()),
		TenantID:              tenant.ID(ids.NewV7().String()),
		OrderID:               consignmentnote.OrderID(ids.NewV7().String()),
		CarrierName:           "BlueDart",
		BoxCount:              3,
		WeightGrams:           7500,
		ExpectedDeliveryAt:    &eta,
		CreatedByMembershipID: membership.ID(ids.NewV7().String()),
		Now:                   fixedNow(),
	}
}

func TestConsignmentNote_New_HappyPath(t *testing.T) {
	t.Parallel()
	cn, err := consignmentnote.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cn.Status() != consignmentnote.StatusPending {
		t.Errorf("status=%s want pending", cn.Status())
	}
	if cn.DocketNumber() != "" {
		t.Errorf("docket=%q want empty on pending", cn.DocketNumber())
	}
	events := cn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}
	if _, ok := events[0].(consignmentnote.CreatedEvent); !ok {
		t.Errorf("event type=%T", events[0])
	}
}

func TestConsignmentNote_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)
	past := fixedNow().Add(-time.Hour)

	cases := []struct {
		name string
		mod  func(*consignmentnote.NewInput)
	}{
		{"zero id", func(in *consignmentnote.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *consignmentnote.NewInput) { in.TenantID = "" }},
		{"zero order", func(in *consignmentnote.NewInput) { in.OrderID = "" }},
		{"empty carrier", func(in *consignmentnote.NewInput) { in.CarrierName = "  " }},
		{"zero box count", func(in *consignmentnote.NewInput) { in.BoxCount = 0 }},
		{"negative box count", func(in *consignmentnote.NewInput) { in.BoxCount = -1 }},
		{"zero weight", func(in *consignmentnote.NewInput) { in.WeightGrams = 0 }},
		{"negative weight", func(in *consignmentnote.NewInput) { in.WeightGrams = -1 }},
		{"zero creator", func(in *consignmentnote.NewInput) { in.CreatedByMembershipID = "" }},
		{"zero now", func(in *consignmentnote.NewInput) { in.Now = time.Time{} }},
		{"eta in past", func(in *consignmentnote.NewInput) { in.ExpectedDeliveryAt = &past }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := consignmentnote.New(in); !errors.Is(err, consignmentnote.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestConsignmentNote_HappyPath(t *testing.T) {
	t.Parallel()
	cn, _ := consignmentnote.New(sampleNewInput(t))
	cn.PullEvents()
	actor := membership.ID(ids.NewV7().String())

	if err := cn.MarkDispatched("BDX-12345", actor, fixedNow().Add(time.Hour)); err != nil {
		t.Fatalf("MarkDispatched: %v", err)
	}
	if cn.Status() != consignmentnote.StatusDispatched {
		t.Errorf("status=%s want dispatched", cn.Status())
	}
	if cn.DocketNumber() != "BDX-12345" {
		t.Errorf("docket=%q", cn.DocketNumber())
	}
	cn.PullEvents()

	if err := cn.MarkInTransit(actor, fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("MarkInTransit: %v", err)
	}
	cn.PullEvents()

	if err := cn.MarkDelivered(actor, fixedNow().Add(3*time.Hour)); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if cn.Status() != consignmentnote.StatusDelivered {
		t.Errorf("status=%s want delivered", cn.Status())
	}
	events := cn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}
	sce, ok := events[0].(consignmentnote.StatusChangedEvent)
	if !ok {
		t.Fatalf("event type=%T", events[0])
	}
	if sce.NewStatus != consignmentnote.StatusDelivered {
		t.Errorf("new status=%s want delivered", sce.NewStatus)
	}
}

func TestConsignmentNote_MarkDispatched_RequiresDocket(t *testing.T) {
	t.Parallel()
	cn, _ := consignmentnote.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())

	if err := cn.MarkDispatched("", actor, fixedNow().Add(time.Hour)); !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("empty docket: got %v want ErrInvalid", err)
	}
	if err := cn.MarkDispatched("   ", actor, fixedNow().Add(time.Hour)); !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("whitespace docket: got %v want ErrInvalid", err)
	}
}

func TestConsignmentNote_MarkDispatched_IdempotentOnSameDocket(t *testing.T) {
	t.Parallel()
	cn, _ := consignmentnote.New(sampleNewInput(t))
	cn.PullEvents()
	actor := membership.ID(ids.NewV7().String())

	_ = cn.MarkDispatched("BDX-99", actor, fixedNow().Add(time.Hour))
	cn.PullEvents()

	// Same docket → no event, no error.
	if err := cn.MarkDispatched("BDX-99", actor, fixedNow().Add(2*time.Hour)); err != nil {
		t.Errorf("re-dispatch same docket: %v", err)
	}
	if got := len(cn.PullEvents()); got != 0 {
		t.Errorf("events=%d want 0 on idempotent re-dispatch", got)
	}

	// Different docket → error (operator typo or wrong carrier).
	if err := cn.MarkDispatched("BDX-100", actor, fixedNow().Add(3*time.Hour)); !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("different docket on re-dispatch: got %v want ErrInvalid", err)
	}
}

func TestConsignmentNote_MarkFailed_FromAnyNonTerminal(t *testing.T) {
	t.Parallel()
	actor := membership.ID(ids.NewV7().String())

	// From pending.
	{
		cn, _ := consignmentnote.New(sampleNewInput(t))
		cn.PullEvents()
		if err := cn.MarkFailed("address not found", actor, fixedNow().Add(time.Hour)); err != nil {
			t.Fatalf("fail from pending: %v", err)
		}
		if cn.Status() != consignmentnote.StatusFailed {
			t.Errorf("status=%s want failed", cn.Status())
		}
		if cn.FailureReason() != "address not found" {
			t.Errorf("reason=%s", cn.FailureReason())
		}
	}

	// From in_transit.
	{
		cn, _ := consignmentnote.New(sampleNewInput(t))
		_ = cn.MarkDispatched("BDX-77", actor, fixedNow().Add(time.Hour))
		_ = cn.MarkInTransit(actor, fixedNow().Add(2*time.Hour))
		cn.PullEvents()

		if err := cn.MarkFailed("damaged in transit", actor, fixedNow().Add(3*time.Hour)); err != nil {
			t.Fatalf("fail from in_transit: %v", err)
		}
		if cn.Status() != consignmentnote.StatusFailed {
			t.Errorf("status=%s", cn.Status())
		}
	}
}

func TestConsignmentNote_TerminalGuards(t *testing.T) {
	t.Parallel()
	actor := membership.ID(ids.NewV7().String())

	// Delivered → cannot fail.
	{
		cn, _ := consignmentnote.New(sampleNewInput(t))
		_ = cn.MarkDispatched("BDX-1", actor, fixedNow().Add(time.Hour))
		_ = cn.MarkDelivered(actor, fixedNow().Add(2*time.Hour))

		if err := cn.MarkFailed("oops", actor, fixedNow().Add(3*time.Hour)); !errors.Is(err, consignmentnote.ErrInvalidTransition) {
			t.Errorf("fail after delivered: got %v want ErrInvalidTransition", err)
		}
		if err := cn.MarkInTransit(actor, fixedNow().Add(3*time.Hour)); !errors.Is(err, consignmentnote.ErrInvalidTransition) {
			t.Errorf("in-transit after delivered: got %v want ErrInvalidTransition", err)
		}
	}

	// Failed → cannot deliver.
	{
		cn, _ := consignmentnote.New(sampleNewInput(t))
		_ = cn.MarkFailed("address bad", actor, fixedNow().Add(time.Hour))
		if err := cn.MarkDelivered(actor, fixedNow().Add(2*time.Hour)); !errors.Is(err, consignmentnote.ErrInvalidTransition) {
			t.Errorf("deliver after failed: got %v want ErrInvalidTransition", err)
		}
	}
}

func TestConsignmentNote_SkipInTransitDirectlyToDelivered(t *testing.T) {
	t.Parallel()
	cn, _ := consignmentnote.New(sampleNewInput(t))
	actor := membership.ID(ids.NewV7().String())
	_ = cn.MarkDispatched("BDX-2", actor, fixedNow().Add(time.Hour))

	// Some carriers skip the in-transit scan + jump dispatched → delivered.
	if err := cn.MarkDelivered(actor, fixedNow().Add(2*time.Hour)); err != nil {
		t.Fatalf("skip in-transit: %v", err)
	}
	if cn.Status() != consignmentnote.StatusDelivered {
		t.Errorf("status=%s", cn.Status())
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"pending", "dispatched", "in_transit", "delivered", "failed"} {
		if _, err := consignmentnote.ParseStatus(ok); err != nil {
			t.Errorf("ParseStatus(%q): %v", ok, err)
		}
	}
	if _, err := consignmentnote.ParseStatus("nonsense"); !errors.Is(err, consignmentnote.ErrInvalid) {
		t.Errorf("ParseStatus bad: got %v want ErrInvalid", err)
	}
}
