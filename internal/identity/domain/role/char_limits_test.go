package role_test

import (
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The column checks length(name) between 2 and 100 characters; the domain counts
// the same unit, so a one-character Devanagari name (3 bytes) is a refusal, not a
// CHECK violation.
func TestRoleName_IsCountedInCharacters(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in string
		ok bool
	}{
		"1 character (3 bytes)": {"अ", false},
		"2 characters":          {"अब", true},
		"100 characters":        {strings.Repeat("अ", role.NameMaxLength), true},
		"101 characters":        {strings.Repeat("अ", role.NameMaxLength+1), false},
	} {
		_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), tc.in, false, 5, false, testNow)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
}
