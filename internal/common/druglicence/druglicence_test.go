package druglicence_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
)

func TestNew_AcceptsValid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"KA-W-22B-12345",
		"MH-21B-12345",
		"20B/12345",
		"DL-22B-12345-2025",
		"AP/22B/00012",
		"AB123456",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			n, err := druglicence.New(raw)
			if err != nil {
				t.Fatalf("New(%q): %v", raw, err)
			}
			if n.String() != raw {
				t.Errorf("String()=%q, want %q", n.String(), raw)
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
		{"too short", "AB12"},
		{"too long", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, // 32 chars
		{"lowercase", "ka-w-22b-12345"},
		{"only letters", "ABCDEFGHIJ"},
		{"only digits", "1234567890"},
		{"underscore", "KA_W_12345"},
		{"period", "KA.W.12345"},
		{"unicode", "KA—W—12345"}, // em-dash not hyphen
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := druglicence.New(tc.raw)
			if !errors.Is(err, druglicence.ErrInvalid) {
				t.Errorf("New(%q): got %v, want druglicence.ErrInvalid", tc.raw, err)
			}
		})
	}
}
