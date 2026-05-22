// Package passwordpolicy holds the password-policy domain contract —
// the [Checker] interface every credential-touching flow consults
// BEFORE persisting a new password hash, plus the [Noop] test double.
//
// Per ADR 0051 (Wave 9.1a): this surface MOVED from
// `internal/common/breach/` to `internal/identity/domain/passwordpolicy/`
// because it was a single-module (Identity) policy living in the
// shared substrate — anti-canon per TDL Wild Workouts single-module
// rule. The interface + test fakes now live with the only consumer
// (Identity); concrete production implementations live in
// `internal/identity/adapters/`.
//
// Two implementations ship today:
//
//   - `passwordpolicy.OfflineList` (in `internal/identity/adapters/`) —
//     the bundled hard-coded weak-password list (OWASP/NIST seed).
//     v0.2 production default. No network; no external deps.
//
//   - [Noop] (this package) — always returns false. Used by tests that
//     don't care about the check (focus on surrounding handler logic);
//     prefer Noop over OfflineList in non-credential-flow tests so the
//     assertions don't accidentally couple to the offline list contents.
//
// Production-grade implementations (HIBP k-anonymity API per Troy Hunt;
// vendor-managed credential databases) plug into the same [Checker]
// interface — wiring-time choice in the composition root.
package passwordpolicy

import (
	"context"
	"errors"
)

// ErrBreached is the sentinel returned by callers (NOT by the checker
// itself) when they've decided to reject the password. The checker
// reports a bool; the application layer decides how to surface the
// rejection. Wrapped per the standard go errors.Is convention so HTTP
// layer can map to 422/400.
var ErrBreached = errors.New("passwordpolicy: password has appeared in known breaches")

// Checker is the domain port. Single-method by design — Mat Ryer 2024
// + Cheney "accept interfaces, return structs": the consumer-side
// declares only what it needs.
//
// Implementations MUST be safe for concurrent use; production callers
// invoke from request handlers under load.
type Checker interface {
	// IsBreached reports whether plaintext has appeared in a known
	// password breach. Returns (true, nil) on breach, (false, nil) on
	// clean, (false, err) on lookup failure (network, ratelimit,
	// transient provider error).
	//
	// Caller MUST handle the error path — failing-open (treating
	// errors as "not breached") would silently weaken security per
	// `security.md`. Recommended: surface as 503 with a retry-after
	// for users; fail-closed but explicit.
	IsBreached(ctx context.Context, plaintext string) (bool, error)
}

// Noop is a [Checker] that never reports a breach. Used by tests that
// don't care about the check; prefer Noop over OfflineList in
// non-credential-flow tests so the test isn't accidentally coupled to
// the offline list contents.
type Noop struct{}

// IsBreached always returns (false, nil).
func (Noop) IsBreached(_ context.Context, _ string) (bool, error) { return false, nil }
