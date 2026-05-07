package breach_test

import (
	"context"
	"testing"

	"github.com/leadkart/leadkart-go/internal/platform/breach"
)

func TestOfflineList_FlagsKnownWeakPasswords(t *testing.T) {
	t.Parallel()
	c := breach.NewOfflineList()
	cases := []string{
		"password",
		"PASSWORD",       // case-insensitive
		" password ",     // outer whitespace stripped
		"password123",
		"123456",
		"qwerty",
		"admin",
		"root",
		"changeme",
		"leadkart",       // LeadKart-specific
		"leadkart_app",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			breached, err := c.IsBreached(context.Background(), raw)
			if err != nil {
				t.Fatalf("IsBreached: %v", err)
			}
			if !breached {
				t.Errorf("IsBreached(%q) = false, want true", raw)
			}
		})
	}
}

func TestOfflineList_AcceptsStrongPasswords(t *testing.T) {
	t.Parallel()
	c := breach.NewOfflineList()
	cases := []string{
		"correct-horse-battery-staple-1234",
		"P@ssw0rd-th@t-1s-actually-strong-9876",
		"7zR9Q$kLmW4#vXp8",
		"my-uncommon-paraphrase-7382",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			breached, err := c.IsBreached(context.Background(), raw)
			if err != nil {
				t.Fatalf("IsBreached: %v", err)
			}
			if breached {
				t.Errorf("IsBreached(%q) = true, want false (strong password)", raw)
			}
		})
	}
}

func TestOfflineList_PreservesInternalWhitespace(t *testing.T) {
	// "correct horse battery staple" is a passphrase — must NOT collapse
	// to "correcthorsebatterystaple" (different cred). The normaliser
	// trims OUTER whitespace only.
	t.Parallel()
	c := breach.NewOfflineListWith([]string{"correcthorsebatterystaple"})
	breached, _ := c.IsBreached(context.Background(), "correct horse battery staple")
	if breached {
		t.Error("internal whitespace should produce different canonicalisation")
	}
}

func TestOfflineList_NewOfflineListWith_FiltersEmpty(t *testing.T) {
	// Empty strings in the seed list MUST NOT make every password
	// look breached.
	t.Parallel()
	c := breach.NewOfflineListWith([]string{"", "  ", "password"})
	if c.Size() != 1 {
		t.Errorf("Size = %d, want 1 (empty entries filtered)", c.Size())
	}
	breached, _ := c.IsBreached(context.Background(), "anything")
	if breached {
		t.Error("non-seeded password reported as breached")
	}
}

func TestOfflineList_DefaultListSize_Sanity(t *testing.T) {
	t.Parallel()
	c := breach.NewOfflineList()
	// Sanity: at least 30 entries — catches an accidental empty-list
	// regression that would silently weaken security.
	if c.Size() < 30 {
		t.Errorf("default offline list too short: %d entries (want ≥30)", c.Size())
	}
}

func TestNoop_NeverReportsBreached(t *testing.T) {
	t.Parallel()
	c := breach.Noop{}
	for _, raw := range []string{"", "password", "123456", "anything"} {
		breached, err := c.IsBreached(context.Background(), raw)
		if err != nil {
			t.Errorf("Noop.IsBreached(%q): err = %v", raw, err)
		}
		if breached {
			t.Errorf("Noop.IsBreached(%q) = true, want false", raw)
		}
	}
}

// Compile-time assertions: both implementations satisfy the Checker
// contract. Drift between implementation + interface surfaces at
// build time, not at first runtime use.
var (
	_ breach.Checker = (*breach.OfflineList)(nil)
	_ breach.Checker = breach.Noop{}
)
