package invoice_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleItem() quotation.LineItem {
	return quotation.LineItem{
		ProductID:     ids.NewV7().String(),
		SKU:           "AMOX-500-T10",
		Description:   "Amoxicillin 500mg Tablet",
		Quantity:      100,
		UnitMrpPaise:  9500,
		UnitSalePaise: 6500,
		GstRateBps:    1200,
	}
}

func sampleNumber() invoicenumber.Number {
	return invoicenumber.MustNew(invoicenumber.KindInvoice, "2026-27", 47)
}

func sampleNewInput(t *testing.T) invoice.NewInput {
	t.Helper()
	return invoice.NewInput{
		ID:        invoice.ID(ids.NewV7().String()),
		TenantID:  tenant.ID(ids.NewV7().String()),
		OrderID:   order.ID(ids.NewV7().String()),
		Number:    sampleNumber(),
		LineItems: []quotation.LineItem{sampleItem()},
		TaxLines: []invoice.TaxLine{{
			HSNCode:           "30041020",
			GSTRateBps:        1200,
			TaxableValuePaise: 650000,
			TaxAmountPaise:    78000,
		}},
		SubtotalPaise:        650000,
		TaxPaise:             78000,
		GrandTotalPaise:      728000,
		IssuedAt:             fixedNow(),
		IssuedByMembershipID: membership.ID(ids.NewV7().String()),
	}
}

func TestInvoice_New_HappyPath(t *testing.T) {
	t.Parallel()
	in := sampleNewInput(t)
	inv, err := invoice.New(in)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if inv.Number().String() != "INV/2026-27/00047" {
		t.Errorf("Number=%s", inv.Number())
	}
	if inv.GrandTotalPaise() != 728000 {
		t.Errorf("GrandTotalPaise=%d", inv.GrandTotalPaise())
	}
	if got := len(inv.LineItems()); got != 1 {
		t.Errorf("LineItems len=%d want 1", got)
	}
}

func TestInvoice_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)

	cases := []struct {
		name string
		mod  func(*invoice.NewInput)
	}{
		{"zero id", func(in *invoice.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *invoice.NewInput) { in.TenantID = "" }},
		{"zero order", func(in *invoice.NewInput) { in.OrderID = "" }},
		{"zero number", func(in *invoice.NewInput) { in.Number = invoicenumber.Number{} }},
		{"wrong number kind", func(in *invoice.NewInput) {
			in.Number = invoicenumber.MustNew(invoicenumber.KindCreditNote, "2026-27", 1)
		}},
		{"empty items", func(in *invoice.NewInput) { in.LineItems = nil }},
		{"grand != subtotal+tax", func(in *invoice.NewInput) { in.GrandTotalPaise = 999999 }},
		{"negative subtotal", func(in *invoice.NewInput) {
			in.SubtotalPaise = -1
			in.GrandTotalPaise = -1 + in.TaxPaise
		}},
		{"zero issued at", func(in *invoice.NewInput) { in.IssuedAt = time.Time{} }},
		{"zero issuer", func(in *invoice.NewInput) { in.IssuedByMembershipID = "" }},
		{"tax line bad hsn", func(in *invoice.NewInput) {
			in.TaxLines = []invoice.TaxLine{{HSNCode: "", GSTRateBps: 1200, TaxableValuePaise: 1, TaxAmountPaise: 1}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			if _, err := invoice.New(in); !errors.Is(err, invoice.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestInvoice_UnmarshalFromDB_Roundtrip(t *testing.T) {
	t.Parallel()
	original, _ := invoice.New(sampleNewInput(t))
	snap := invoice.Snapshot{
		ID:                   original.ID(),
		TenantID:             original.TenantID(),
		OrderID:              original.OrderID(),
		Number:               original.Number(),
		LineItems:            original.LineItems(),
		TaxLines:             original.TaxLines(),
		SubtotalPaise:        original.SubtotalPaise(),
		TaxPaise:             original.TaxPaise(),
		GrandTotalPaise:      original.GrandTotalPaise(),
		IssuedAt:             original.IssuedAt(),
		IssuedByMembershipID: original.IssuedByMembershipID(),
	}
	hydrated := invoice.UnmarshalFromDB(snap)
	if hydrated.Number().String() != original.Number().String() {
		t.Errorf("number mismatch after roundtrip")
	}
	if len(hydrated.LineItems()) != len(original.LineItems()) {
		t.Errorf("line items count mismatch")
	}
}
