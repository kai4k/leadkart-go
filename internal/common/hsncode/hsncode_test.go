package hsncode_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/hsncode"
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"3004", "300410", "30041020",
		"0402", "040210", "04021010", // chapters 01-09 are real: a leading zero is allowed
		"0101", "0000", // structurally valid; the master table is not consulted
	} {
		c, err := hsncode.New(raw)
		if err != nil {
			t.Errorf("New(%q): %v", raw, err)
		}
		if c.String() != raw {
			t.Errorf("New(%q).String() = %s", raw, c.String())
		}
	}
}

func TestNew_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{" 3004 ": "3004", "\t30041020\n": "30041020", " 0402": "0402"} {
		c, err := hsncode.New(in)
		if err != nil || c.String() != want {
			t.Errorf("New(%q) = %q, %v; want %q", in, c, err, want)
		}
	}
}

func TestNew_Rejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",           // empty
		"   ",        // blank
		"3",          // too short
		"30",         // a bare chapter is not a product code
		"300",        // 3 digits
		"30041",      // 5 digits
		"3004102",    // 7 digits
		"300410200",  // 9 digits
		"3004102000", // 10 digits (the old Product rule accepted it)
		"30041020X",  // alpha tail
		"abc",        // non-numeric
		"30 04",      // inner space
		"+3004",      // sign
		"3004.10",    // separator
		"３００４",       // full-width digits are not ASCII digits
		"٣٠٠٤",       // Arabic-Indic digits
	}
	for _, raw := range cases {
		if _, err := hsncode.New(raw); !errors.Is(err, hsncode.ErrInvalid) {
			t.Errorf("New(%q): got %v want ErrInvalid", raw, err)
		}
	}
}

func TestMustNew_PanicsOnInvalid(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = hsncode.MustNew("garbage") // arch-test:ignore-err -- MustNew returns Code (no error); test asserts the panic, not the discarded value.
}

func TestAccessors(t *testing.T) {
	t.Parallel()
	full := hsncode.MustNew("30041020")
	if got := full.Chapter(); got != "30" {
		t.Errorf("Chapter=%s want 30", got)
	}
	if got := full.Heading(); got != "3004" {
		t.Errorf("Heading=%s want 3004", got)
	}
	if got := full.Subheading(); got != "300410" {
		t.Errorf("Subheading=%s want 300410", got)
	}
	if got := full.Length(); got != 8 {
		t.Errorf("Length=%d want 8", got)
	}

	// Shorter code — accessors return empty for unavailable layers.
	heading := hsncode.MustNew("3004")
	if heading.Subheading() != "" {
		t.Errorf("4-digit code Subheading=%q want empty", heading.Subheading())
	}
	if heading.Chapter() != "30" {
		t.Errorf("4-digit code Chapter=%q want 30", heading.Chapter())
	}
}

func TestIsPharma(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"3004", "300410", "30041020"} {
		if !hsncode.MustNew(raw).IsPharma() {
			t.Errorf("%s.IsPharma()=false want true (Chapter 30)", raw)
		}
	}
	// Non-pharma chapters.
	for _, raw := range []string{"2201", "8517", "84713010", "0402"} {
		if hsncode.MustNew(raw).IsPharma() {
			t.Errorf("%s.IsPharma()=true want false", raw)
		}
	}
}

func TestIsZero(t *testing.T) {
	t.Parallel()
	if !hsncode.Code("").IsZero() {
		t.Error("empty IsZero()=false")
	}
	if hsncode.MustNew("3004").IsZero() {
		t.Error("non-empty IsZero()=true")
	}
}
