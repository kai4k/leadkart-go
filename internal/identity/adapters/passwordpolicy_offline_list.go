// passwordpolicy_offline_list.go — bundled [passwordpolicy.Checker]
// backed by a hard-coded weak-password set (ADR 0051, Wave 9.1a).
// Production deployments can swap in an HIBP k-anonymity checker at
// composition time via the same interface.

package adapters

import (
	"context"
	"errors"
	"strings"

	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
)

// OfflinePasswordList implements [passwordpolicy.Checker] using a
// hard-coded set of common weak passwords. Lookups are O(1) and
// case-insensitive; no network dependency. Source: OWASP Auth Cheat Sheet
// 2025, NIST SP 800-63B §5.1.1.2, and credential-stuffing datasets.
type OfflinePasswordList struct {
	set map[string]struct{}
}

// NewOfflinePasswordList constructs the default offline checker.
func NewOfflinePasswordList() *OfflinePasswordList {
	return &OfflinePasswordList{set: defaultBreachedSet()}
}

// NewOfflinePasswordListWith constructs an OfflinePasswordList from the
// supplied seed (for tests). Inputs are lowercased and trimmed; empty
// strings are silently skipped.
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

// normalisePassword lowercases and trims outer whitespace. Internal
// whitespace is preserved (passphrases differ from their collapsed forms).
func normalisePassword(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// defaultBreachedSet is the seed shipped with OfflinePasswordList.
// Append-only; never remove an entry without operator review.
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
