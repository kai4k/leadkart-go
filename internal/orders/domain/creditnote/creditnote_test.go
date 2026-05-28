package creditnote_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote/creditnotetest"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleCDN(t *testing.T, tID tenant.ID, invID invoice.ID, kind invoicenumber.Kind, seq int64) *creditnote.CreditNote {
	t.Helper()
	n := invoicenumber.MustNew(kind, "2026-27", seq)
	in := creditnote.NewInput{
		ID:                 creditnote.ID(ids.NewV7().String()),
		TenantID:           tID,
		InvoiceID:          invID,
		Number:             n,
		Kind:               kind,
		Reason:             "customer return",
		AmountPaise:        50000,
		IssuedAt:           fixedNow(),
		IssuedByMembership: membership.ID(ids.NewV7().String()),
	}
	c, err := creditnote.New(in)
	if err != nil {
		t.Fatalf("sample creditnote: %v", err)
	}
	return c
}

func TestCreditNote_New_HappyPath(t *testing.T) {
	t.Parallel()
	tID := tenant.ID(ids.NewV7().String())
	invID := invoice.ID(ids.NewV7().String())
	c := sampleCDN(t, tID, invID, invoicenumber.KindCreditNote, 1)
	if c.Kind() != invoicenumber.KindCreditNote {
		t.Errorf("kind=%s", c.Kind())
	}
	if c.Number().String() != "CDN/2026-27/00001" {
		t.Errorf("number=%s", c.Number())
	}
}

func TestCreditNote_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := creditnote.NewInput{
		ID:                 creditnote.ID(ids.NewV7().String()),
		TenantID:           tenant.ID(ids.NewV7().String()),
		InvoiceID:          invoice.ID(ids.NewV7().String()),
		Number:             invoicenumber.MustNew(invoicenumber.KindCreditNote, "2026-27", 1),
		Kind:               invoicenumber.KindCreditNote,
		Reason:             "test",
		AmountPaise:        100,
		IssuedAt:           fixedNow(),
		IssuedByMembership: membership.ID(ids.NewV7().String()),
	}
	cases := []struct {
		name string
		mod  func(*creditnote.NewInput)
	}{
		{"zero id", func(in *creditnote.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *creditnote.NewInput) { in.TenantID = "" }},
		{"zero invoice", func(in *creditnote.NewInput) { in.InvoiceID = "" }},
		{"zero number", func(in *creditnote.NewInput) { in.Number = invoicenumber.Number{} }},
		{"invalid kind invoice", func(in *creditnote.NewInput) { in.Kind = invoicenumber.KindInvoice }},
		{"number kind mismatch", func(in *creditnote.NewInput) {
			in.Kind = invoicenumber.KindCancellationNote
			// in.Number remains KindCreditNote
		}},
		{"empty reason", func(in *creditnote.NewInput) { in.Reason = "   " }},
		{"zero amount", func(in *creditnote.NewInput) { in.AmountPaise = 0 }},
		{"negative amount", func(in *creditnote.NewInput) { in.AmountPaise = -1 }},
		{"zero issued at", func(in *creditnote.NewInput) { in.IssuedAt = time.Time{} }},
		{"zero issuer", func(in *creditnote.NewInput) { in.IssuedByMembership = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := creditnote.New(in); !errors.Is(err, creditnote.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestFakeRepository_CancellationUniquenessPerInvoice(t *testing.T) {
	t.Parallel()
	repo := creditnotetest.NewFakeRepository()
	tID := tenant.ID(ids.NewV7().String())
	invID := invoice.ID(ids.NewV7().String())

	// First CancellationNote → ok.
	first := sampleCDN(t, tID, invID, invoicenumber.KindCancellationNote, 1)
	if err := repo.Add(t.Context(), first); err != nil {
		t.Fatalf("first CN: %v", err)
	}

	// Second CancellationNote for the SAME invoice → ErrCancellationAlreadyExists.
	second := sampleCDN(t, tID, invID, invoicenumber.KindCancellationNote, 2)
	if err := repo.Add(t.Context(), second); !errors.Is(err, creditnote.ErrCancellationAlreadyExists) {
		t.Errorf("second CN: got %v want ErrCancellationAlreadyExists", err)
	}

	// CreditNote (post-delivery return) for the SAME invoice → ok (partial returns stack).
	cdn := sampleCDN(t, tID, invID, invoicenumber.KindCreditNote, 1)
	if err := repo.Add(t.Context(), cdn); err != nil {
		t.Errorf("CDN with existing CN: %v", err)
	}

	// Another CreditNote → ok (partial returns stack).
	cdn2 := sampleCDN(t, tID, invID, invoicenumber.KindCreditNote, 2)
	if err := repo.Add(t.Context(), cdn2); err != nil {
		t.Errorf("second CDN: %v", err)
	}

	// ListByInvoice returns all 3 (CN + 2 CDNs).
	list, err := repo.ListByInvoice(t.Context(), tID, invID)
	if err != nil {
		t.Fatalf("ListByInvoice: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("ListByInvoice len=%d want 3", len(list))
	}
}
