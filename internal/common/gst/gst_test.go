package gst_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/gst"
)

func TestNew_AcceptsValidGSTIN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		raw       string
		stateCode int
		pan       string
	}{
		{"karnataka 1A", "29ABCDE1234F1Z5", 29, "ABCDE1234F"},
		{"delhi 1Z", "07XYZAB5678C2Z9", 7, "XYZAB5678C"},
		{"tn first state 01", "01PQRST9999D9Z0", 1, "PQRST9999D"},
		{"max active 37", "37ABCDE1234F1Z5", 37, "ABCDE1234F"},
		{"other territory 97", "97ABCDE1234F1Z5", 97, "ABCDE1234F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := gst.New(tc.raw)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.raw, err)
			}
			if n.String() != tc.raw {
				t.Errorf("String()=%q, want %q", n.String(), tc.raw)
			}
			if n.StateCode() != tc.stateCode {
				t.Errorf("StateCode()=%d, want %d", n.StateCode(), tc.stateCode)
			}
			if n.PAN() != tc.pan {
				t.Errorf("PAN()=%q, want %q", n.PAN(), tc.pan)
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
		{"too short", "29ABCDE1234F1Z"},
		{"too long", "29ABCDE1234F1Z55"},
		{"lowercase", "29abcde1234f1z5"},
		{"missing Z at 14", "29ABCDE1234F1A5"},
		{"state 00", "00ABCDE1234F1Z5"},
		{"state 38", "38ABCDE1234F1Z5"},
		{"state 96 not 97", "96ABCDE1234F1Z5"},
		{"non-digit state", "AAABCDE1234F1Z5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := gst.New(tc.raw)
			if !errors.Is(err, gst.ErrInvalid) {
				t.Errorf("New(%q): got %v, want gst.ErrInvalid", tc.raw, err)
			}
		})
	}
}

func TestZeroAccessors(t *testing.T) {
	t.Parallel()
	var n gst.Number
	if !n.IsZero() {
		t.Error("zero value not IsZero")
	}
	if n.StateCode() != 0 {
		t.Errorf("zero StateCode() = %d", n.StateCode())
	}
	if n.PAN() != "" {
		t.Errorf("zero PAN() = %q", n.PAN())
	}
}
