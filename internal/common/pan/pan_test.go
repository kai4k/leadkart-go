package pan_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/pan"
)

func TestNew_AcceptsValidPAN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		raw        string
		entityType byte
	}{
		{"individual", "ABCPE1234F", 'P'},
		{"firm", "ABCFE1234G", 'F'},
		{"company", "ABCCE1234H", 'C'},
		{"hindu undivided family", "ABCHE1234I", 'H'},
		{"trust", "ABCTE1234J", 'T'},
		{"government", "ABCGE1234K", 'G'},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n, err := pan.New(tc.raw)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.raw, err)
			}
			if n.String() != tc.raw {
				t.Errorf("String()=%q, want %q", n.String(), tc.raw)
			}
			if n.EntityType() != tc.entityType {
				t.Errorf("EntityType()=%c, want %c", n.EntityType(), tc.entityType)
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
		{"too short", "ABCPE1234"},
		{"too long", "ABCPE1234FX"},
		{"lowercase", "abcpe1234f"},
		{"digit in alpha section", "ABC1E1234F"},
		{"alpha in digit section", "ABCPEX234F"},
		{"invalid entity type X", "ABCXE1234F"},  // X not in PFHCATBLJG
		{"invalid entity type Z", "ABCZE1234F"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := pan.New(tc.raw)
			if !errors.Is(err, pan.ErrInvalid) {
				t.Errorf("New(%q): got %v, want pan.ErrInvalid", tc.raw, err)
			}
		})
	}
}

func TestZeroAccessors(t *testing.T) {
	t.Parallel()
	var n pan.Number
	if !n.IsZero() {
		t.Error("zero value not IsZero")
	}
	if n.EntityType() != 0 {
		t.Errorf("zero EntityType()=%d", n.EntityType())
	}
}
