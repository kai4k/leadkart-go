package rolehierarchy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func newEdgeWithReason(reason string) (*rolehierarchy.Edge, error) {
	return rolehierarchy.New(rolehierarchy.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()), role.ID(ids.NewV7().String()), membership.ID(ids.NewV7().String()), reason, fixedClock())
}

// The columns check length(reason) between 10 and 1024 characters (reason and
// removal_reason alike): the domain counts characters, not UTF-8 bytes.
func TestReason_IsCountedInCharacters(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reason string
		ok     bool
	}{
		"9 characters (27 bytes)":      {strings.Repeat("अ", rolehierarchy.MinReasonLength-1), false},
		"10 characters (30 bytes)":     {strings.Repeat("अ", rolehierarchy.MinReasonLength), true},
		"max characters":               {strings.Repeat("अ", rolehierarchy.MaxReasonLength), true},
		"max characters plus one":      {strings.Repeat("अ", rolehierarchy.MaxReasonLength+1), false},
		"blank is no reason, and fine": {"   ", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newEdgeWithReason(tc.reason)
			if tc.ok != (err == nil) || (err != nil && !errors.Is(err, rolehierarchy.ErrInvalidReason)) {
				t.Errorf("create: err = %v, want ok=%v", err, tc.ok)
			}
			e, err := newEdgeWithReason("manager reorg requires this parent link")
			if err != nil {
				t.Fatal(err)
			}
			err = e.Remove(membership.ID(ids.NewV7().String()), tc.reason, fixedClock())
			if tc.ok != (err == nil) || (err != nil && !errors.Is(err, rolehierarchy.ErrInvalidReason)) {
				t.Errorf("remove: err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
