// passwordpolicy_offline_list.go — bundled in-memory implementation
// of [passwordpolicy.Checker] backed by a hard-coded set of well-known
// weak passwords.
//
// Per ADR 0051 (Wave 9.1a): the interface lives in
// `internal/identity/domain/passwordpolicy/`; the concrete impl lives
// here in the adapters package alongside the other repository +
// gateway implementations. Cheney "accept interfaces, return structs"
// + TDL Wild Workouts single-module rule.
//
// Production-grade implementations (HIBP k-anonymity API per Troy
// Hunt) plug into the same [passwordpolicy.Checker] interface —
// wiring-time choice in the composition root.

package adapters

import (
	"context"
	"errors"
	"strings"

	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
)

// OfflinePasswordList is the bundled minimum-viable [passwordpolicy.Checker]
// backed by a hard-coded set of well-known weak passwords. Lookups
// are O(1) + case-insensitive. No network, no external dependencies —
// safe for dev + early-deploy production.
//
// Source list compiled from:
//   - OWASP Authentication Cheat Sheet 2025 "Common weak passwords"
//   - NIST SP 800-63B 2025 §5.1.1.2 "compromised password screening"
//     example list
//   - Top entries from public credential-stuffing datasets
//
// Catches the casual cases — "password", "12345678", "qwerty" — and
// the LeadKart-default placeholders ("change_me", "leadkart"). For
// production-grade coverage, swap to an HIBPChecker when the API
// integration lands (deferred to v1.0).
type OfflinePasswordList struct {
	set map[string]struct{}
}

// NewOfflinePasswordList constructs the default offline checker.
func NewOfflinePasswordList() *OfflinePasswordList {
	return &OfflinePasswordList{set: defaultBreachedSet()}
}

// NewOfflinePasswordListWith constructs an OfflinePasswordList seeded
// by the supplied passwords. Used by tests that want to assert
// specific entries are rejected/accepted; production calls
// [NewOfflinePasswordList].
//
// Inputs are normalised: lowercased + trimmed. Empty strings are
// silently skipped (don't allow "" to mark every password as breached).
func NewOfflinePasswordListWith(passwords []string) *OfflinePasswordList {
	set := make(map[string]struct{}, len(passwords))
	for _, p := range passwords {
		k := normalisePassword(p)
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	return &OfflinePasswordList{set: set}
}

// Compile-time interface satisfaction.
var _ passwordpolicy.Checker = (*OfflinePasswordList)(nil)

// IsBreached reports whether plaintext (after case-fold + trim) is in
// the offline set.
func (c *OfflinePasswordList) IsBreached(_ context.Context, plaintext string) (bool, error) {
	if c == nil {
		return false, errors.New("passwordpolicy: OfflinePasswordList is nil")
	}
	_, breached := c.set[normalisePassword(plaintext)]
	return breached, nil
}

// Size returns the number of entries — useful for /health reporting +
// sanity-check tests.
func (c *OfflinePasswordList) Size() int {
	if c == nil {
		return 0
	}
	return len(c.set)
}

// normalisePassword canonicalises a password for set membership: trim
// outer whitespace + lowercase. Internal whitespace is preserved (a
// passphrase like "correct horse battery staple" is genuinely different
// from "correcthorsebatterystaple").
func normalisePassword(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// defaultBreachedSet returns the seed set OfflinePasswordList ships
// with. Kept short + auditable here rather than in a separate data
// file — a curated set is more useful than an exhaustive one
// (developers can SEE what's banned without grepping a CSV).
//
// Update policy: append only; never remove an entry without operator
// review (could re-enable a banned password unintentionally).
func defaultBreachedSet() map[string]struct{} {
	list := []string{
		// OWASP top-N + universal classics
		"password",
		"password1",
		"password123",
		"123456",
		"12345678",
		"123456789",
		"1234567890",
		"qwerty",
		"qwerty123",
		"qwertyuiop",
		"abc123",
		"111111",
		"000000",
		"letmein",
		"welcome",
		"monkey",
		"dragon",
		"master",
		"baseball",
		"football",
		"iloveyou",
		"sunshine",
		"princess",
		"shadow",
		"superman",
		"trustno1",

		// Operations / sysadmin classics
		"admin",
		"administrator",
		"root",
		"toor",
		"guest",
		"changeme",
		"change_me",
		"changeit",
		"default",
		"test123",
		"test1234",
		"demo",
		"demo123",

		// LeadKart-specific (catches placeholder values from dev seeds)
		"leadkart",
		"leadkart123",
		"leadkart_app",
		"leadkart_app_password",
		"leadkart_test_password",
		"leadkart_test",
		"leadkart_dev",

		// Indian-context common weak passwords
		"india123",
		"bharat123",
		"pharma123",
		"ramesh",
		"krishna",
		"krishna123",
	}
	set := make(map[string]struct{}, len(list))
	for _, p := range list {
		set[normalisePassword(p)] = struct{}{}
	}
	return set
}
