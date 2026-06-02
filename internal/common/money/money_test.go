package money_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/money"
)

func TestCurrency_IsValid_And_Symbol(t *testing.T) {
	t.Parallel()
	cases := map[money.Currency]string{
		money.INR: "₹",
		money.USD: "$",
		money.EUR: "€",
	}
	for c, sym := range cases {
		if !c.IsValid() {
			t.Errorf("%s.IsValid()=false", c)
		}
		if c.Symbol() != sym {
			t.Errorf("%s.Symbol()=%s want %s", c, c.Symbol(), sym)
		}
	}
	if money.Currency("XYZ").IsValid() {
		t.Errorf("unknown currency.IsValid()=true want false")
	}
	if money.Currency("XYZ").Symbol() != "" {
		t.Errorf("unknown currency.Symbol() returned non-empty")
	}
}

func TestCurrency_MinorUnitsPerMajor(t *testing.T) {
	t.Parallel()
	for _, c := range []money.Currency{money.INR, money.USD, money.EUR} {
		if got := c.MinorUnitsPerMajor(); got != 100 {
			t.Errorf("%s.MinorUnitsPerMajor()=%d want 100", c, got)
		}
	}
	if money.Currency("XYZ").MinorUnitsPerMajor() != 0 {
		t.Errorf("unknown currency.MinorUnitsPerMajor() returned non-zero")
	}
}

func TestFormat_INR_LakhsCroresStyle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		paise int64
		want  string
	}{
		{0, "₹0.00"},
		{50, "₹0.50"},
		{100, "₹1.00"},
		{12345, "₹123.45"},
		{99999, "₹999.99"},
		{100000, "₹1,000.00"},                  // 1 thousand
		{1234567, "₹12,345.67"},                // ten thousand range
		{12345678, "₹1,23,456.78"},             // one lakh — INR grouping kicks in
		{1234567890, "₹1,23,45,678.90"},        // crore territory
		{123456789012, "₹1,23,45,67,890.12"},   // big number — multiple groups of 2
	}
	for _, c := range cases {
		got := money.Format(c.paise, money.INR)
		if got != c.want {
			t.Errorf("Format(%d, INR) = %q want %q", c.paise, got, c.want)
		}
	}
}

func TestFormat_Negative(t *testing.T) {
	t.Parallel()
	cases := map[int64]string{
		-100:      "₹-1.00",
		-12345678: "₹-1,23,456.78",
	}
	for paise, want := range cases {
		got := money.Format(paise, money.INR)
		if got != want {
			t.Errorf("Format(%d) = %q want %q", paise, got, want)
		}
	}
}

func TestFormat_USDEURGroupOfThree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		paise int64
		curr  money.Currency
		want  string
	}{
		{1234567, money.USD, "$12,345.67"},
		{123456789, money.USD, "$1,234,567.89"},
		{123456789, money.EUR, "€1,234,567.89"},
	}
	for _, c := range cases {
		got := money.Format(c.paise, c.curr)
		if got != c.want {
			t.Errorf("Format(%d, %s) = %q want %q", c.paise, c.curr, got, c.want)
		}
	}
}

func TestFormat_UnknownCurrencyFallback(t *testing.T) {
	t.Parallel()
	got := money.Format(12345, money.Currency("XYZ"))
	want := "12345 XYZ"
	if got != want {
		t.Errorf("Format(12345, XYZ) = %q want %q", got, want)
	}
}

func TestFormatPaiseINR_Shorthand(t *testing.T) {
	t.Parallel()
	if money.FormatPaiseINR(12345678) != money.Format(12345678, money.INR) {
		t.Error("FormatPaiseINR shorthand mismatch")
	}
}

func TestParsePaise_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want int64
	}{
		{"₹1,23,456.78", 12345678},
		{"1,23,456.78", 12345678},
		{"₹12345678", 1234567800}, // no separators, no fractional
		{"₹0.00", 0},
		{"₹0", 0},
		{"₹0.5", 50},                // one fractional digit padded
		{"₹0.50", 50},
		{"₹0.555", 55},              // rounded to two digits (truncated)
		{"  ₹ 1,000.00  ", 100000},  // whitespace tolerance
		{"-₹1.00", -100},            // negative outside symbol
		{"₹-1.00", -100},            // negative inside (Indian banking style)
	}
	for _, c := range cases {
		got, err := money.ParsePaise(c.raw)
		if err != nil {
			t.Errorf("ParsePaise(%q): %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePaise(%q) = %d want %d", c.raw, got, c.want)
		}
	}
}

func TestParsePaise_Rejects(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"garbage",
		"₹abc",
		"₹1.2.3",     // two decimal points
		"₹1,2,3.bad", // bad fractional
	} {
		if _, err := money.ParsePaise(raw); !errors.Is(err, money.ErrInvalid) {
			t.Errorf("ParsePaise(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestFormat_Roundtrip_Idempotent(t *testing.T) {
	t.Parallel()
	for _, paise := range []int64{0, 100, 12345, 12345678, -12345678, 1234567890} {
		formatted := money.Format(paise, money.INR)
		got, err := money.ParsePaise(formatted)
		if err != nil {
			t.Errorf("roundtrip ParsePaise(%q): %v", formatted, err)
			continue
		}
		if got != paise {
			t.Errorf("roundtrip %d → %q → %d (mismatch)", paise, formatted, got)
		}
	}
}
