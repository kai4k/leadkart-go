package payment_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment/paymenttest"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleNewInput(t *testing.T) payment.NewInput {
	t.Helper()
	return payment.NewInput{
		ID:                   payment.ID(ids.NewV7().String()),
		TenantID:             tenant.ID(ids.NewV7().String()),
		OrderID:              order.ID(ids.NewV7().String()),
		Kind:                 payment.KindToken,
		Method:               payment.MethodUPI,
		AmountPaise:          50000,
		ExternalReference:    "UPI-REF-001",
		Notes:                "10% token",
		ReceivedAt:           fixedNow(),
		RecordedAt:           fixedNow().Add(time.Minute),
		RecordedByMembership: membership.ID(ids.NewV7().String()),
	}
}

func TestPayment_New_HappyPath(t *testing.T) {
	t.Parallel()
	p, err := payment.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Kind() != payment.KindToken {
		t.Errorf("kind=%s", p.Kind())
	}
	if p.AmountPaise() != 50000 {
		t.Errorf("amount=%d", p.AmountPaise())
	}
	if p.ExternalReference() != "UPI-REF-001" {
		t.Errorf("ref=%s", p.ExternalReference())
	}
}

func TestPayment_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)
	cases := []struct {
		name string
		mod  func(*payment.NewInput)
	}{
		{"zero id", func(in *payment.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *payment.NewInput) { in.TenantID = "" }},
		{"zero order", func(in *payment.NewInput) { in.OrderID = "" }},
		{"bad kind", func(in *payment.NewInput) { in.Kind = "nonsense" }},
		{"bad method", func(in *payment.NewInput) { in.Method = "btc" }},
		{"zero amount", func(in *payment.NewInput) { in.AmountPaise = 0 }},
		{"negative amount", func(in *payment.NewInput) { in.AmountPaise = -1 }},
		{"zero received at", func(in *payment.NewInput) { in.ReceivedAt = time.Time{} }},
		{"zero recorded at", func(in *payment.NewInput) { in.RecordedAt = time.Time{} }},
		{"recorded before received", func(in *payment.NewInput) {
			in.RecordedAt = in.ReceivedAt.Add(-time.Hour)
		}},
		{"zero recorder", func(in *payment.NewInput) { in.RecordedByMembership = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := payment.New(in); !errors.Is(err, payment.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestFakeRepository_DedupOnExternalReference(t *testing.T) {
	t.Parallel()
	repo := paymenttest.NewFakeRepository()

	in := sampleNewInput(t)
	first, _ := payment.New(in)
	if err := repo.Add(t.Context(), first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Same ExternalReference + tenant → reject.
	in2 := in
	in2.ID = payment.ID(ids.NewV7().String())
	second, _ := payment.New(in2)
	if err := repo.Add(t.Context(), second); !errors.Is(err, payment.ErrAlreadyExistsForExternalReference) {
		t.Errorf("dup external ref: got %v want ErrAlreadyExistsForExternalReference", err)
	}

	// Empty ExternalReference → no dedup (manual entry, operator-trusted).
	in3 := in
	in3.ID = payment.ID(ids.NewV7().String())
	in3.ExternalReference = ""
	third, _ := payment.New(in3)
	if err := repo.Add(t.Context(), third); err != nil {
		t.Errorf("empty-ref Add: %v (should not dedup)", err)
	}

	// Another empty-ref one → still allowed.
	in4 := in3
	in4.ID = payment.ID(ids.NewV7().String())
	fourth, _ := payment.New(in4)
	if err := repo.Add(t.Context(), fourth); err != nil {
		t.Errorf("second empty-ref Add: %v", err)
	}
}

func TestFakeRepository_ListByOrder(t *testing.T) {
	t.Parallel()
	repo := paymenttest.NewFakeRepository()
	tID := tenant.ID(ids.NewV7().String())
	orderID := order.ID(ids.NewV7().String())

	for i, kind := range []payment.Kind{payment.KindToken, payment.KindFull} {
		in := sampleNewInput(t)
		in.TenantID = tID
		in.OrderID = orderID
		in.ID = payment.ID(ids.NewV7().String())
		in.Kind = kind
		in.ExternalReference = fmtRef(i)
		in.ReceivedAt = fixedNow().Add(time.Duration(i) * time.Hour)
		in.RecordedAt = in.ReceivedAt.Add(time.Minute)
		p, _ := payment.New(in)
		if err := repo.Add(t.Context(), p); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	list, err := repo.ListByOrder(t.Context(), tID, orderID)
	if err != nil {
		t.Fatalf("ListByOrder: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d want 2", len(list))
	}
	if list[0].Kind() != payment.KindToken {
		t.Errorf("first kind=%s want token", list[0].Kind())
	}
	if list[1].Kind() != payment.KindFull {
		t.Errorf("second kind=%s want full", list[1].Kind())
	}
}

func fmtRef(i int) string {
	return "REF-" + string(rune('A'+i))
}
