package membership_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
)

func TestStatus_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    membership.Status
		want string
	}{
		{membership.StatusUnknown, "unknown"},
		{membership.StatusActive, "active"},
		{membership.StatusInactive, "inactive"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		want    membership.Status
		wantErr bool
	}{
		{"active", "active", membership.StatusActive, false},
		{"inactive", "inactive", membership.StatusInactive, false},
		{"unknown", "unknown", membership.StatusUnknown, true},
		{"empty", "", membership.StatusUnknown, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := membership.ParseStatus(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.raw)
				}
				if !errors.Is(err, membership.ErrInvalid) {
					t.Errorf("expected errors.Is ErrInvalid")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
