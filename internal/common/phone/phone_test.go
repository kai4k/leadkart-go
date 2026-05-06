package phone_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/phone"
)

func TestNew_AcceptsValidE164(t *testing.T) {
	t.Parallel()
	cases := []string{
		"+919876543210",         // Indian
		"+14155551234",          // US
		"+447700900123",         // UK
		"+8612345678901",        // China (13 digits)
		"+19999999999999",       // 14 digits
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, err := phone.New(raw)
			if err != nil {
				t.Fatalf("New(%q): %v", raw, err)
			}
			if got.String() != raw {
				t.Errorf("String() = %q, want %q", got.String(), raw)
			}
			if got.IsZero() {
				t.Error("IsZero() = true for valid number")
			}
		})
	}
}

func TestNew_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"no plus", "919876543210"},
		{"too short", "+1234567"},                // 7 digits after +
		{"too long", "+1234567890123456"},        // 16 digits after +
		{"contains space", "+91 9876543210"},
		{"contains hyphen", "+91-9876543210"},
		{"contains parens", "+91(987)6543210"},
		{"leading zero", "+09876543210"},
		{"non digit", "+91987a543210"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := phone.New(tc.raw)
			if !errors.Is(err, phone.ErrInvalid) {
				t.Errorf("New(%q): got %v, want phone.ErrInvalid", tc.raw, err)
			}
		})
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a, _ := phone.New("+919876543210")
	b, _ := phone.New("+919876543210")
	c, _ := phone.New("+919999999999")

	if !a.Equal(b) {
		t.Error("a should equal b")
	}
	if a.Equal(c) {
		t.Error("a should not equal c")
	}
}

func TestZeroValueIsZero(t *testing.T) {
	t.Parallel()
	var n phone.Number
	if !n.IsZero() {
		t.Error("zero value should report IsZero")
	}
	if n.String() != "" {
		t.Errorf("zero String() = %q, want empty", n.String())
	}
}
