package passwordpolicy_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
)

func TestNoop_NeverReportsBreached(t *testing.T) {
	t.Parallel()
	c := passwordpolicy.Noop{}
	for _, raw := range []string{"", "password", "123456", "anything"} {
		breached, err := c.IsBreached(t.Context(), raw)
		if err != nil {
			t.Errorf("Noop.IsBreached(%q): err = %v", raw, err)
		}
		if breached {
			t.Errorf("Noop.IsBreached(%q) = true, want false", raw)
		}
	}
}

// Compile-time assertion that the in-domain test fake satisfies the
// [passwordpolicy.Checker] contract. Drift between Noop + the
// interface surfaces at build time, not at first runtime use.
var _ passwordpolicy.Checker = passwordpolicy.Noop{}
