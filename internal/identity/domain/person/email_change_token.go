package person

import (
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
)

// EmailChangeTokenHash is the SHA-256 hex of the opaque confirmation
// token emailed to the Person's NEW email address. The aggregate
// stores ONLY the hash; the raw token leaves the domain via the
// application-layer integration event (EmailChangeRequestedV1) for
// email delivery and is never persisted.
//
// Mirrors [PasswordResetTokenHash] — same envelope. Same hashing
// canon in the application layer (SHA-256 of base64url-encoded
// random bytes).
type EmailChangeTokenHash struct {
	value string
}

const (
	emailChangeHashMinLen = 32
	emailChangeHashMaxLen = 128
)

// NewEmailChangeTokenHash wraps a hash string. Validates length +
// non-emptiness only.
func NewEmailChangeTokenHash(raw string) (EmailChangeTokenHash, error) {
	if raw == "" {
		return EmailChangeTokenHash{}, fmt.Errorf("%w: email change token hash empty", ErrInvalid)
	}
	if len(raw) < emailChangeHashMinLen {
		return EmailChangeTokenHash{}, fmt.Errorf(
			"%w: email change token hash too short (got %d, want ≥%d)",
			ErrInvalid, len(raw), emailChangeHashMinLen,
		)
	}
	if len(raw) > emailChangeHashMaxLen {
		return EmailChangeTokenHash{}, fmt.Errorf(
			"%w: email change token hash too long (got %d, want ≤%d)",
			ErrInvalid, len(raw), emailChangeHashMaxLen,
		)
	}
	return EmailChangeTokenHash{value: raw}, nil
}

// String returns the underlying hex.
func (h EmailChangeTokenHash) String() string { return h.value }

// IsZero reports whether the hash is the empty zero value.
func (h EmailChangeTokenHash) IsZero() bool { return h.value == "" }

// Equal performs constant-time comparison.
func (h EmailChangeTokenHash) Equal(other EmailChangeTokenHash) bool {
	return subtle.ConstantTimeCompare([]byte(h.value), []byte(other.value)) == 1
}

// PendingEmailChange is the per-Person sub-state recorded by
// [Person.RequestEmailChange]. Zero value means "no pending change."
//
// Carries the proposed new email + the token hash + expiry. The new
// email is applied to Person.email ONLY when ConfirmEmailChange is
// invoked with a matching token within the window.
//
// At-most-one-pending invariant: a fresh RequestEmailChange supersedes
// any prior pending change — same canon as PendingPasswordReset.
type PendingEmailChange struct {
	newEmail  email.Address
	hash      EmailChangeTokenHash
	expiresAt time.Time
}

// IsZero reports whether no email change is pending.
func (p PendingEmailChange) IsZero() bool { return p.hash.IsZero() }

// NewEmail returns the proposed new email address.
func (p PendingEmailChange) NewEmail() email.Address { return p.newEmail }

// Hash returns the stored token hash.
func (p PendingEmailChange) Hash() EmailChangeTokenHash { return p.hash }

// ExpiresAt returns the absolute expiry timestamp.
func (p PendingEmailChange) ExpiresAt() time.Time { return p.expiresAt }
