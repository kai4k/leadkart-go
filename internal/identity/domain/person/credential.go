package person

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// ErrInvalid is the sentinel for person-domain validation failures.
//
// Declared here (in credential.go rather than person.go) so the test
// file can reference it before the Person struct is defined; standard Go
// import resolution makes the location a stylistic choice only.
var ErrInvalid = errs.New(errs.KindInvalidInput, "person", "invalid person")

// PasswordHash is the opaque Argon2id-hashed password.
//
// Treated as opaque BYTES at the domain layer — the Argon2id wrapper
// (in adapters/auth or platform/auth) produces this from a plaintext
// password + verifies it on login. Domain doesn't know hashing internals.
//
// Stored as PHC string (`$argon2id$v=19$m=...,t=...,p=...$salt$hash`)
// per Argon2 reference implementation. Never compare via string equality
// at the domain level — always via the verifier.
type PasswordHash struct {
	value string
}

// hashMinLen is a sanity floor — real Argon2id PHC strings are always far
// longer (~96+ chars including salt + hash). This catches obviously empty
// or corrupted inputs before they reach the verifier.
const hashMinLen = 40

// NewPasswordHash wraps a hash string from the Argon2id adapter.
//
// Treats the string as opaque — does NOT parse PHC format here. The
// hashing adapter (Argon2idHasher) is responsible for producing valid
// PHC; this constructor only checks non-emptiness + minimum plausibility.
func NewPasswordHash(raw string) (PasswordHash, error) {
	if raw == "" {
		return PasswordHash{}, fmt.Errorf("%w: password hash empty", ErrInvalid)
	}
	if len(raw) < hashMinLen {
		return PasswordHash{}, fmt.Errorf("%w: password hash suspiciously short (got %d, want ≥%d)", ErrInvalid, len(raw), hashMinLen)
	}
	return PasswordHash{value: raw}, nil
}

// String returns the underlying PHC string. Only the persistence adapter
// + verifier should call this — never log or surface in HTTP responses.
func (h PasswordHash) String() string { return h.value }

// IsZero reports whether the hash was constructed.
func (h PasswordHash) IsZero() bool { return h.value == "" }

// SecurityStamp is a per-Person opaque token that rotates on every
// auth-relevant change (password change, role assignment, email change,
// logout-all). JWT issuance embeds the current stamp; subsequent requests
// validate the JWT's stamp matches the stored one.
//
// When the stamp rotates, all outstanding JWTs become invalid on next
// request without requiring server-side blacklist tracking.
//
// Implemented as UUIDv7 — collision-free + time-ordered for forensic
// "when did this user last rotate" queries.
type SecurityStamp [16]byte

// NewSecurityStamp generates a fresh stamp.
func NewSecurityStamp() SecurityStamp {
	id := ids.NewV7()
	return SecurityStamp(id)
}

// SecurityStampFromString parses a UUID string back into a stamp.
// Used by the persistence adapter on read paths.
func SecurityStampFromString(raw string) (SecurityStamp, error) {
	u, err := uuid.Parse(raw)
	if err != nil {
		return SecurityStamp{}, fmt.Errorf("%w: invalid security stamp %q: %v", ErrInvalid, raw, err)
	}
	return SecurityStamp(u), nil
}

// String returns the canonical UUID string form for serialisation.
func (s SecurityStamp) String() string {
	return uuid.UUID(s).String()
}

// IsZero reports whether the stamp is the zero value.
func (s SecurityStamp) IsZero() bool {
	var zero SecurityStamp
	return s == zero
}

// Equal reports whether two stamps match. Used in JWT validation to
// compare the embedded stamp against the stored one.
func (s SecurityStamp) Equal(other SecurityStamp) bool {
	return s == other
}
