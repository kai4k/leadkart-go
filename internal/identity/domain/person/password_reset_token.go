package person

import (
	"crypto/subtle"
	"fmt"
	"time"
)

// PasswordResetTokenHash is the SHA-256 hex of the opaque reset token
// emailed to the Person. The aggregate stores ONLY the hash; the raw
// token leaves the domain via the application-layer integration event
// (PasswordResetRequestedV1) for email delivery and is never persisted.
//
// Mirrors the [refreshtoken.TokenHash] shape — same SHA-256 hex
// envelope so the two token surfaces share canonical hashing in the
// application layer.
//
// Length policy: SHA-256 hex = 64 chars. We accept 32–128 to leave
// headroom for migrating to longer-output hashes (BLAKE3, SHA-512)
// without an aggregate-method signature change.
type PasswordResetTokenHash struct {
	value string
}

const (
	resetHashMinLen = 32
	resetHashMaxLen = 128
)

// NewPasswordResetTokenHash wraps a hash string. Validates length +
// non-emptiness only — content shape (hex / base64url) is the
// hashing-side concern.
func NewPasswordResetTokenHash(raw string) (PasswordResetTokenHash, error) {
	if raw == "" {
		return PasswordResetTokenHash{}, fmt.Errorf("%w: password reset token hash empty", ErrInvalid)
	}
	if len(raw) < resetHashMinLen {
		return PasswordResetTokenHash{}, fmt.Errorf(
			"%w: reset token hash too short (got %d, want ≥%d)",
			ErrInvalid, len(raw), resetHashMinLen,
		)
	}
	if len(raw) > resetHashMaxLen {
		return PasswordResetTokenHash{}, fmt.Errorf(
			"%w: reset token hash too long (got %d, want ≤%d)",
			ErrInvalid, len(raw), resetHashMaxLen,
		)
	}
	return PasswordResetTokenHash{value: raw}, nil
}

// String returns the underlying hex.
func (h PasswordResetTokenHash) String() string { return h.value }

// IsZero reports whether the hash is the empty zero value.
func (h PasswordResetTokenHash) IsZero() bool { return h.value == "" }

// Equal performs constant-time comparison. Reset-token verification is
// a security boundary — fast equality is a timing-leak risk against
// the stored hash.
func (h PasswordResetTokenHash) Equal(other PasswordResetTokenHash) bool {
	return subtle.ConstantTimeCompare([]byte(h.value), []byte(other.value)) == 1
}

// PendingPasswordReset is the per-Person sub-state recorded by
// [Person.RequestPasswordReset]. Zero value means "no pending reset."
//
// At-most-one-pending invariant per Person: a fresh
// [Person.RequestPasswordReset] supersedes any prior pending reset
// (the new hash + expiry overwrite). This matches Auth0 / Okta /
// Microsoft Entra ID canonical password-reset semantics — the most
// recent request wins, the previous email is silently invalidated.
type PendingPasswordReset struct {
	hash      PasswordResetTokenHash
	expiresAt time.Time
}

// IsZero reports whether no reset is pending.
func (p PendingPasswordReset) IsZero() bool { return p.hash.IsZero() }

// Hash returns the stored token hash.
func (p PendingPasswordReset) Hash() PasswordResetTokenHash { return p.hash }

// ExpiresAt returns the absolute expiry timestamp.
func (p PendingPasswordReset) ExpiresAt() time.Time { return p.expiresAt }
