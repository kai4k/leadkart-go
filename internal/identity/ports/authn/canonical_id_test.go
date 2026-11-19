package authn_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// CanonicalID delegates to the one canonical-id function (ids.Canonical, which
// owns the exhaustive table); this pins the (string, bool) adapter shape.
func TestCanonicalID_DelegatesToCanonical(t *testing.T) {
	t.Parallel()
	got, ok := authn.CanonicalID("0190A000-0000-7000-8000-00000000000A")
	if !ok || got != "0190a000-0000-7000-8000-00000000000a" {
		t.Fatalf("CanonicalID(upper) = %q, %v", got, ok)
	}
	for _, bad := range []string{"", " 0190a000-0000-7000-8000-00000000000a", "00000000-0000-0000-0000-000000000000", "nope"} {
		if got, ok := authn.CanonicalID(bad); ok || got != "" {
			t.Errorf("CanonicalID(%q) = %q, %v; want \"\", false", bad, got, ok)
		}
	}
}
