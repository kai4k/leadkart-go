package invoicenumber_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

func TestParseFinancialYear_HappyPath(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"2026-27", "2099-00", "2000-01"} {
		got, err := invoicenumber.ParseFinancialYear(raw)
		if err != nil {
			t.Errorf("ParseFinancialYear(%q) err=%v want nil", raw, err)
		}
		if got.String() != raw {
			t.Errorf("ParseFinancialYear(%q) got %s", raw, got)
		}
	}
}

func TestParseFinancialYear_Rejects(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"2026",
		"2026-26",       // tail not next-year mod 100
		"2026-2027",     // wrong format
		"2026/27",       // wrong separator
		"FY26-27",       // not numeric
		"2026-50",       // bad tail
	} {
		if _, err := invoicenumber.ParseFinancialYear(raw); !errors.Is(err, invoicenumber.ErrInvalid) {
			t.Errorf("ParseFinancialYear(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestFromDate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		date time.Time
		want invoicenumber.FinancialYear
	}{
		{time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), "2026-27"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "2026-27"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2026-27"},
		{time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC), "2026-27"},
		{time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), "2027-28"},
		{time.Date(2099, 4, 1, 0, 0, 0, 0, time.UTC), "2099-00"},
	}
	for _, c := range cases {
		got := invoicenumber.FromDate(c.date)
		if got != c.want {
			t.Errorf("FromDate(%s) got %s want %s", c.date.Format(time.DateOnly), got, c.want)
		}
	}
}

func TestNumber_Format(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind invoicenumber.Kind
		fy   invoicenumber.FinancialYear
		seq  int64
		want string
	}{
		{invoicenumber.KindInvoice, "2026-27", 47, "INV/2026-27/00047"},
		{invoicenumber.KindCreditNote, "2026-27", 1, "CDN/2026-27/00001"},
		{invoicenumber.KindCancellationNote, "2026-27", 1234, "CN/2026-27/01234"},
		{invoicenumber.KindInvoice, "2026-27", 99999, "INV/2026-27/99999"},
		{invoicenumber.KindInvoice, "2026-27", 100000, "INV/2026-27/100000"}, // overflow widens
	}
	for _, c := range cases {
		n, err := invoicenumber.New(c.kind, c.fy, c.seq)
		if err != nil {
			t.Fatalf("New(%s, %s, %d): %v", c.kind, c.fy, c.seq, err)
		}
		if got := n.String(); got != c.want {
			t.Errorf("Number(%s, %s, %d).String() got %s want %s", c.kind, c.fy, c.seq, got, c.want)
		}
		if n.Kind() != c.kind || n.FinancialYear() != c.fy || n.Seq() != c.seq {
			t.Errorf("Number getters mismatch for %s/%s/%d", c.kind, c.fy, c.seq)
		}
	}
}

func TestNumber_Rejects(t *testing.T) {
	t.Parallel()
	if _, err := invoicenumber.New(invoicenumber.Kind("garbage"), "2026-27", 1); !errors.Is(err, invoicenumber.ErrInvalid) {
		t.Errorf("invalid kind: got %v want ErrInvalid", err)
	}
	if _, err := invoicenumber.New(invoicenumber.KindInvoice, "", 1); !errors.Is(err, invoicenumber.ErrInvalid) {
		t.Errorf("zero fy: got %v want ErrInvalid", err)
	}
	if _, err := invoicenumber.New(invoicenumber.KindInvoice, "2026-27", 0); !errors.Is(err, invoicenumber.ErrInvalid) {
		t.Errorf("zero seq: got %v want ErrInvalid", err)
	}
	if _, err := invoicenumber.New(invoicenumber.KindInvoice, "2026-27", -1); !errors.Is(err, invoicenumber.ErrInvalid) {
		t.Errorf("negative seq: got %v want ErrInvalid", err)
	}
}

func TestFormat_ConvenienceMatchesNumberString(t *testing.T) {
	t.Parallel()
	got, err := invoicenumber.Format(invoicenumber.KindInvoice, "2026-27", 47)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	want := "INV/2026-27/00047"
	if got != want {
		t.Errorf("Format got %s want %s", got, want)
	}
}

func TestKindPrefix(t *testing.T) {
	t.Parallel()
	cases := map[invoicenumber.Kind]string{
		invoicenumber.KindInvoice:          "INV",
		invoicenumber.KindCreditNote:       "CDN",
		invoicenumber.KindCancellationNote: "CN",
	}
	for k, want := range cases {
		if got := k.Prefix(); got != want {
			t.Errorf("Prefix(%s) got %s want %s", k, got, want)
		}
	}
	if got := invoicenumber.Kind("nonsense").Prefix(); got != "" {
		t.Errorf("Prefix(nonsense) got %s want empty", got)
	}
}
