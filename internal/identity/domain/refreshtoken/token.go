package refreshtoken

import (
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
)

// ErrInvalid is the sentinel for refresh-token domain validation failures.
var ErrInvalid = errs.New(errs.KindInvalidInput, "refreshtoken", "invalid refresh token")

// TokenID is the per-token primary key (UUIDv7). Distinct from FamilyID —
// each rotation generation gets its own TokenID even within the same family.
type TokenID string

// IsZero reports whether the ID is unset.
func (i TokenID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i TokenID) String() string { return string(i) }

// Equal reports whether two TokenIDs match.
func (i TokenID) Equal(other TokenID) bool { return i == other }

// TokenHash is the SHA-256 hex of an opaque refresh-token string.
//
// The opaque token itself NEVER touches the database — only the hash. This
// matches Auth0/Okta canonical refresh-token storage shape: a leak of the
// DB doesn't expose live tokens; only hashes (which are useless to an
// attacker without the matching plaintext token).
//
// Stored as 64-char hex string (SHA-256 output). Validated for length +
// non-emptiness here; the cryptographic generation lives in the auth
// adapter.
type TokenHash struct {
	value string
}

const (
	hashMinLen = 40 // sanity floor — real SHA-256 hex is 64 chars
	hashMaxLen = 256
)

// NewTokenHash wraps a hash string. Validates length + non-emptiness only.
func NewTokenHash(raw string) (TokenHash, error) {
	if raw == "" {
		return TokenHash{}, fmt.Errorf("%w: token hash empty", ErrInvalid)
	}
	if len(raw) < hashMinLen {
		return TokenHash{}, fmt.Errorf("%w: token hash too short (got %d, want ≥%d)", ErrInvalid, len(raw), hashMinLen)
	}
	if len(raw) > hashMaxLen {
		return TokenHash{}, fmt.Errorf("%w: token hash too long (got %d, want ≤%d)", ErrInvalid, len(raw), hashMaxLen)
	}
	return TokenHash{value: raw}, nil
}

// String returns the underlying hex string. Adapter-only consumer.
func (h TokenHash) String() string { return h.value }

// IsZero reports whether the hash was constructed.
func (h TokenHash) IsZero() bool { return h.value == "" }

// Equal reports whether two hashes match (constant-time NOT required —
// both sides have already been normalised to hex).
func (h TokenHash) Equal(other TokenHash) bool { return h.value == other.value }

// Token represents one generation in a refresh-token family chain.
//
// Lifecycle of a token:
//  1. Issued (IssuedAt set; ConsumedAt + ReplacedByID zero).
//  2. Used to authenticate via Family.Rotate → ConsumedAt set, ReplacedByID
//     points to the new generation's TokenID.
//  3. After consumption, the token is dead — presenting it again triggers
//     reuse detection and revokes the entire family (RFC 9700 §4.13).
type Token struct {
	id           TokenID
	hash         TokenHash
	generation   int32
	issuedAt     time.Time
	expiresAt    time.Time
	consumedAt   time.Time
	replacedByID TokenID
}

// ID returns the token primary key.
func (t Token) ID() TokenID { return t.id }

// Hash returns the SHA-256 hash of the opaque token string.
func (t Token) Hash() TokenHash { return t.hash }

// Generation returns the rotation index (0 = first).
func (t Token) Generation() int32 { return t.generation }

// IssuedAt returns the issuance timestamp.
func (t Token) IssuedAt() time.Time { return t.issuedAt }

// ExpiresAt returns the absolute expiry (family-relative TTL applied at issuance).
func (t Token) ExpiresAt() time.Time { return t.expiresAt }

// ConsumedAt returns when this token was rotated; zero if still current.
func (t Token) ConsumedAt() time.Time { return t.consumedAt }

// ReplacedByID returns the TokenID of the next-generation token; zero if current.
func (t Token) ReplacedByID() TokenID { return t.replacedByID }

// IsConsumed reports whether the token has been rotated out.
func (t Token) IsConsumed() bool { return !t.consumedAt.IsZero() }

// IsExpired reports whether the token has passed its absolute expiry.
func (t Token) IsExpired(now time.Time) bool {
	return !t.expiresAt.IsZero() && now.After(t.expiresAt)
}

// TokenSnapshot is the persistence DTO consumed by [UnmarshalFromDB].
type TokenSnapshot struct {
	ID           TokenID
	Hash         TokenHash
	Generation   int32
	IssuedAt     time.Time
	ExpiresAt    time.Time
	ConsumedAt   time.Time
	ReplacedByID TokenID
}
