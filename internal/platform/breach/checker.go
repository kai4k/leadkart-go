// Package breach defines the password-breach checker port — the
// abstraction every credential-touching flow consults BEFORE persisting
// a new password hash. Per LeadKart .NET parent's IPasswordBreachChecker
// + `security.md` "HIBP+Argon2id+JWT".
//
// Two implementations ship today:
//
//   - [OfflineList] — a small in-memory set of well-known weak
//     passwords ("password", "qwerty123", "admin"...). Sufficient for
//     dev + integration tests + first-deploy production until the
//     HIBP API integration lands. Per OWASP "Authentication Cheat
//     Sheet" 2025 §7.1: even a top-100 list catches the bulk of
//     credential-stuffing victims.
//
//   - [Noop] — always returns false (not breached). Used by tests
//     that don't care about the check (focus on the surrounding
//     handler logic) without leaking the OfflineList behaviour into
//     unrelated assertions.
//
// Production-grade implementations (HIBP k-anonymity API per Troy
// Hunt; vendor-managed credential databases) plug into the same
// [Checker] interface — wiring-time choice in the composition root.
//
// The decorator pattern from `coding-standards.md` "Cross-cutting
// policy per service" wraps the breach check around the password
// hasher: the password-touching handler injects ONE [argon2.Hasher]
// and a Scrutor-style decorator runs the breach check before
// delegating. That decorator lives in
// `internal/identity/app/passwordhash/` (added when the first
// password-mutating endpoint lands in A.3); this package only
// provides the underlying [Checker] surface.
package breach

import (
	"context"
	"errors"
	"strings"
)

// ErrBreached is returned by [Checker.IsBreached] callers (NOT by
// the checker itself) when they've decided to reject the password.
// The checker reports a bool; the application layer decides how to
// surface the rejection. Wrapped per the standard go errors.Is
// convention so HTTP layer can map to 422/400.
var ErrBreached = errors.New("breach: password has appeared in known breaches")

// Checker is the port. Single-method by design — Mat Ryer 2024 +
// Cheney "accept interfaces, return structs": the consumer-side
// declares only what it needs.
//
// Implementations MUST be safe for concurrent use; production callers
// invoke from request handlers under load.
type Checker interface {
	// IsBreached reports whether plaintext has appeared in a known
	// password breach. Returns (true, nil) on breach, (false, nil)
	// on clean, (false, err) on lookup failure (network, ratelimit,
	// transient provider error).
	//
	// Caller MUST handle the error path — failing-open (treating
	// errors as "not breached") would silently weaken security
	// per `security.md`. Recommended: surface as 503 with a
	// retry-after for users; fail-closed but explicit.
	IsBreached(ctx context.Context, plaintext string) (bool, error)
}

// ----- OfflineList implementation ------------------------------------

// OfflineList is the bundled minimum-viable [Checker] backed by a
// hard-coded set of well-known weak passwords. Lookups are O(1) +
// case-insensitive. No network, no external dependencies — safe for
// dev + early-deploy production.
//
// Source list compiled from:
//   - OWASP Authentication Cheat Sheet 2025 "Common weak passwords"
//   - NIST SP 800-63B 2025 §5.1.1.2 "compromised password screening"
//     example list
//   - Top entries from public credential-stuffing datasets
//
// Catches the casual cases — "password", "12345678", "qwerty" — and
// the LeadKart-default placeholders ("change_me", "leadkart"). For
// production-grade coverage, swap to an [HIBPChecker] when the API
// integration lands (deferred to v1.0).
type OfflineList struct {
	set map[string]struct{}
}

// NewOfflineList constructs the default offline checker.
func NewOfflineList() *OfflineList {
	return &OfflineList{set: defaultBreachedSet()}
}

// NewOfflineListWith constructs an OfflineList seeded by the supplied
// passwords. Used by tests that want to assert specific entries are
// rejected/accepted; production calls [NewOfflineList].
//
// Inputs are normalised: lowercased + trimmed. Empty strings are
// silently skipped (don't allow "" to mark every password as breached).
func NewOfflineListWith(passwords []string) *OfflineList {
	set := make(map[string]struct{}, len(passwords))
	for _, p := range passwords {
		k := normalise(p)
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	return &OfflineList{set: set}
}

// IsBreached reports whether plaintext (after case-fold + trim) is in
// the offline set.
func (c *OfflineList) IsBreached(_ context.Context, plaintext string) (bool, error) {
	if c == nil {
		return false, errors.New("breach: OfflineList is nil")
	}
	_, breached := c.set[normalise(plaintext)]
	return breached, nil
}

// Size returns the number of entries — useful for /health reporting
// + sanity-check tests.
func (c *OfflineList) Size() int {
	if c == nil {
		return 0
	}
	return len(c.set)
}

// ----- Noop implementation -------------------------------------------

// Noop is a [Checker] that never reports a breach. Used by tests that
// don't care about the check; prefer this over OfflineList in
// non-credential-flow tests so the test isn't accidentally coupled to
// the offline list contents.
type Noop struct{}

// IsBreached always returns (false, nil).
func (Noop) IsBreached(_ context.Context, _ string) (bool, error) { return false, nil }

// ----- Helpers -------------------------------------------------------

// normalise canonicalises a password for set membership: trim outer
// whitespace + lowercase. Internal whitespace is preserved (a passphrase
// like "correct horse battery staple" is genuinely different from
// "correcthorsebatterystaple").
func normalise(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// defaultBreachedSet returns the seed set OfflineList ships with.
// Kept short + auditable here rather than in a separate data file —
// a curated set is more useful than an exhaustive one (developers can
// SEE what's banned without grepping a CSV).
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
		set[normalise(p)] = struct{}{}
	}
	return set
}
