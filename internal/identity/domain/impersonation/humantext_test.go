package impersonation_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

func TestNewSession_RefusesAReasonWithDirectionOverrideOrControlCharacters(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		"ticket 4711 " + string(rune(0x202e)) + "reordered",
		"ticket 4711\nsecond line",
		"ticket 4711 " + string(rune(0x200b)) + "zero width",
	} {
		_, err := impersonation.NewSession("op-1", "tenant-1", reason, 0, fixedNow)
		var he *humantext.Error
		if !errors.As(err, &he) || he.Field != "reason" {
			t.Errorf("reason %q: err = %v, want humantext.Error for reason", reason, err)
		}
	}
	if _, err := impersonation.NewSession("op-1", "tenant-1", "ticket 4711 diagnose onboarding", 0, fixedNow); err != nil {
		t.Errorf("a clean reason: %v", err)
	}
}
