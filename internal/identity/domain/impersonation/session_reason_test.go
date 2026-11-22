package impersonation_test

import (
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// The audit reason is measured and stored AFTER trimming: ten spaces are no reason.
func TestNewSession_ReasonIsTrimmedBeforeTheLengthCheckAndStoredTrimmed(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{strings.Repeat(" ", 10), strings.Repeat(" ", 40), "\t\t\t\t\t\t\t\t\t\t", "   short    ", "  abcdefghi  "} {
		if _, err := impersonation.NewSession("op-1", "tenant-1", blank, 0, fixedNow); err == nil {
			t.Errorf("reason %q (%d bytes, fewer than 10 once trimmed) opened a session", blank, len(blank))
		}
	}
	s, err := impersonation.NewSession("op-1", "tenant-1", "   investigating ticket 4711   ", 0, fixedNow)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s.Reason() != "investigating ticket 4711" {
		t.Errorf("stored reason = %q, want it trimmed", s.Reason())
	}
	if _, err := impersonation.NewSession("op-1", "tenant-1", "  abcdefghij  ", 0, fixedNow); err != nil {
		t.Errorf("exactly 10 characters once trimmed was refused: %v", err)
	}
}
