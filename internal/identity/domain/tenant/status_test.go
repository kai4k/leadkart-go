package tenant_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestStatus_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    tenant.Status
		want string
	}{
		{tenant.StatusUnknown, "unknown"},
		{tenant.StatusPending, "pending"},
		{tenant.StatusActive, "active"},
		{tenant.StatusSuspended, "suspended"},
		{tenant.StatusPendingDeletion, "pending_deletion"},
		{tenant.StatusDeleted, "deleted"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.s.String(); got != tc.want {
				t.Errorf("Status(%d).String() = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     string
		want    tenant.Status
		wantErr bool
	}{
		{"pending", "pending", tenant.StatusPending, false},
		{"active", "active", tenant.StatusActive, false},
		{"suspended", "suspended", tenant.StatusSuspended, false},
		{"pending_deletion", "pending_deletion", tenant.StatusPendingDeletion, false},
		{"deleted", "deleted", tenant.StatusDeleted, false},
		{"unknown", "unknown", tenant.StatusUnknown, true},
		{"empty", "", tenant.StatusUnknown, true},
		{"junk", "junk", tenant.StatusUnknown, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tenant.ParseStatus(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.raw)
				}
				if !errors.Is(err, tenant.ErrInvalid) {
					t.Errorf("expected errors.Is ErrInvalid, got %v", err)
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
