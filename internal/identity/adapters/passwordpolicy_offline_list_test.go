package adapters_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
)

func TestOfflinePasswordList_FlagsKnownWeakPasswords(t *testing.T) {
	t.Parallel()
	c := adapters.NewOfflinePasswordList()
	cases := []string{
		"password",
		"PASSWORD",   // case-insensitive
		" password ", // outer whitespace stripped
		"password123",
		"123456",
		"qwerty",
		"admin",
		"root",
		"changeme",
		"leadkart", // LeadKart-specific
		"leadkart_app",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			breached, err := c.IsBreached(t.Context(), raw)
			if err != nil {
				t.Fatalf("IsBreached: %v", err)
			}
			if !breached {
				t.Errorf("IsBreached(%q) = false, want true", raw)
			}
		})
	}
}

func TestOfflinePasswordList_AcceptsStrongPasswords(t *testing.T) {
	t.Parallel()
	c := adapters.NewOfflinePasswordList()
	cases := []string{
		"correct-horse-battery-staple-1234",
		"P@ssw0rd-th@t-1s-actually-strong-9876",
		"7zR9Q$kLmW4#vXp8",
		"my-uncommon-paraphrase-7382",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			breached, err := c.IsBreached(t.Context(), raw)
			if err != nil {
				t.Fatalf("IsBreached: %v", err)
			}
			if breached {
				t.Errorf("IsBreached(%q) = true, want false (strong password)", raw)
			}
		})
	}
}

func TestOfflinePasswordList_PreservesInternalWhitespace(t *testing.T) {
	// "correct horse battery staple" is a passphrase — must NOT collapse
	// to "correcthorsebatterystaple" (different cred). The normaliser
	// trims OUTER whitespace only.
	t.Parallel()
	c := adapters.NewOfflinePasswordListWith([]string{"correcthorsebatterystaple"})
	breached, _ := c.IsBreached(t.Context(), "correct horse battery staple")
	if breached {
		t.Error("internal whitespace should produce different canonicalisation")
	}
}

func TestOfflinePasswordList_NewOfflinePasswordListWith_FiltersEmpty(t *testing.T) {
	// Empty strings in the seed list MUST NOT make every password look
	// breached.
	t.Parallel()
	c := adapters.NewOfflinePasswordListWith([]string{"", "  ", "password"})
	if c.Size() != 1 {
		t.Errorf("Size = %d, want 1 (empty entries filtered)", c.Size())
	}
	breached, _ := c.IsBreached(t.Context(), "anything")
	if breached {
		t.Error("non-seeded password reported as breached")
	}
}

func TestOfflinePasswordList_DefaultListSize_Sanity(t *testing.T) {
	t.Parallel()
	c := adapters.NewOfflinePasswordList()
	// Sanity: at least 30 entries — catches an accidental empty-list
	// regression that would silently weaken security.
	if c.Size() < 30 {
		t.Errorf("default offline list too short: %d entries (want ≥30)", c.Size())
	}
}
