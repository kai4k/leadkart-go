package hsncode_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/hsncode"
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"30", "3004", "300410", "30041020"} {
		c, err := hsncode.New(raw)
		if err != nil {
			t.Errorf("New(%q): %v", raw, err)
		}
		if c.String() != raw {
			t.Errorf("New(%q).String() = %s", raw, c.String())
		}
	}
}

func TestNew_Rejects(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",          // empty
		"3",         // too short
		"300",       // odd length
		"30041",     // odd length
		"30041020X", // alpha tail
		"030041",    // first digit 0
		"abc",       // non-numeric
		"300410200", // too long (9 digits)
		"30 04",     // space
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
	chap := hsncode.MustNew("30")
	if chap.Heading() != "" {
		t.Errorf("2-digit code Heading=%q want empty", chap.Heading())
	}
	if chap.Subheading() != "" {
		t.Errorf("2-digit code Subheading=%q want empty", chap.Subheading())
	}
}

func TestIsPharma(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"30", "3004", "30041020"} {
		if !hsncode.MustNew(raw).IsPharma() {
			t.Errorf("%s.IsPharma()=false want true (Chapter 30)", raw)
		}
	}
	// Non-pharma chapters.
	for _, raw := range []string{"22", "8517", "84713010"} {
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
	if hsncode.MustNew("30").IsZero() {
		t.Error("non-empty IsZero()=true")
	}
}
