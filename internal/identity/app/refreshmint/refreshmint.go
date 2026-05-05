// Package refreshmint mints opaque refresh tokens — the secret string
// returned to the client, paired with the SHA-256 hash stored on
// [refreshtoken.Token].
//
// Per RFC 9700 §4.13 + Auth0/Okta canon: refresh tokens are 256-bit
// crypto/rand bytes, base64url-encoded for transport, hash-only-stored
// at the persistence layer. The plaintext NEVER hits Postgres.
//
// Two artefacts per mint:
//
//   - Plaintext (returned to client, displayed once, never re-derivable).
//   - Hash (stored — single indexed lookup at rotation time).
package refreshmint

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// PlaintextLen is the byte count of the un-encoded random source.
// 32 bytes = 256 bits — the RFC 9700-recommended minimum entropy.
const PlaintextLen = 32

// Pair is the ⟨plaintext, hash⟩ tuple a single mint produces.
//
// Plaintext is the URL-safe base64-encoded random string returned to
// the HTTP client. Hash is the hex-encoded SHA-256 of the plaintext —
// what gets stored on the [refreshtoken.Token] aggregate.
type Pair struct {
	Plaintext string
	Hash      refreshtoken.TokenHash
}

// Mint generates a fresh ⟨plaintext, hash⟩ pair backed by crypto/rand.
//
// Caller stores Hash on the new Token; returns Plaintext to the client
// in the same response that returns the access token. Subsequent
// refresh-rotation flows present the plaintext, which the server hashes
// and looks up via [refreshtoken.Repository.GetByTokenHash].
func Mint() (Pair, error) {
	buf := make([]byte, PlaintextLen)
	if _, err := rand.Read(buf); err != nil {
		return Pair{}, fmt.Errorf("refreshmint: rand: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(buf)
	hash, err := refreshtoken.NewTokenHash(HashOf(plaintext))
	if err != nil {
		return Pair{}, fmt.Errorf("refreshmint: wrap hash: %w", err)
	}
	return Pair{Plaintext: plaintext, Hash: hash}, nil
}

// HashOf returns the hex-encoded SHA-256 of the supplied plaintext.
// Used by the rotation flow: client presents plaintext → server
// computes HashOf → repository lookup.
func HashOf(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
