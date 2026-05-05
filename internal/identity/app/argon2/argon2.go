// Package argon2 wraps golang.org/x/crypto/argon2 with the LeadKart
// password-hashing parameters per OWASP Password Storage 2025 + RFC 9106.
//
// Hash output is the canonical PHC string format
//
//	$argon2id$v=19$m=19456,t=2,p=1$<base64-salt>$<base64-hash>
//
// which is the same format the .NET LeadKart side stored, so re-hashing
// on first login works without an explicit migration column. Verify
// returns ErrMismatch on wrong password (treated as auth failure
// upstream); ErrFormat when the stored string isn't a recognised PHC.
package argon2

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Defaults follows OWASP Password Storage Cheat Sheet (2025) Argon2id
// floor: memory=19456 KiB (~19 MiB), iterations=2, parallelism=1, and
// 32-byte output. {RFC 9106 §4 + OWASP recommendation; LeadKart .NET
// security.md "Password hashing".}
const (
	Memory     uint32 = 19 * 1024 // KiB
	Iterations uint32 = 2
	Parallel   uint8  = 1
	SaltLen    uint32 = 16
	KeyLen     uint32 = 32
)

// ErrMismatch is returned by [Verify] when the supplied password does
// not match the hash. Upstream auth code maps this to a generic
// 401 "invalid credentials" response (no enumeration distinction
// between "unknown email" and "wrong password").
var ErrMismatch = errors.New("argon2: password mismatch")

// ErrFormat is returned by [Verify] when the stored hash isn't a
// recognised Argon2id PHC string. Indicates data corruption OR a
// foreign hash format leaked into the table — should be operator-alerted.
var ErrFormat = errors.New("argon2: invalid hash format")

// Hash produces a fresh Argon2id PHC string for password.
//
// Per RFC 9106: each call generates a fresh 16-byte salt; salt + params
// + hash are encoded in the standard PHC format so future Verify calls
// recover the params without a separate metadata column.
func Hash(password string) (string, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2: rand: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, Iterations, Memory, Parallel, KeyLen)
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		Memory, Iterations, Parallel,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// Verify checks whether password produces stored — returns nil on match,
// [ErrMismatch] on mismatch, or [ErrFormat] if stored is malformed.
//
// Constant-time hash comparison via crypto/subtle to defeat timing
// oracles. Upstream MUST also dummy-verify on unknown-email paths to
// flatten timing per LeadKart .NET security.md "Login flow" canon.
func Verify(password, stored string) error {
	memory, iterations, parallel, salt, want, err := parsePHC(stored)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallel, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// parsePHC decodes a `$argon2id$v=19$m=...,t=...,p=...$salt$hash` string.
// Returns the cost parameters + raw salt + raw hash. Strict — extra
// segments, unknown variants, or wrong version → [ErrFormat].
func parsePHC(s string) (memory, iterations uint32, parallel uint8, salt, hash []byte, err error) {
	parts := strings.Split(s, "$")
	// Expected: ["", "argon2id", "v=19", "m=19456,t=2,p=1", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		err = ErrFormat
		return
	}

	if !strings.HasPrefix(parts[2], "v=") {
		err = ErrFormat
		return
	}
	version, perr := strconv.ParseInt(strings.TrimPrefix(parts[2], "v="), 10, 32)
	if perr != nil || int(version) != argon2.Version {
		err = ErrFormat
		return
	}

	costs := strings.Split(parts[3], ",")
	if len(costs) != 3 {
		err = ErrFormat
		return
	}
	mem, perr := parseKV(costs[0], "m=")
	if perr != nil {
		err = ErrFormat
		return
	}
	iter, perr := parseKV(costs[1], "t=")
	if perr != nil {
		err = ErrFormat
		return
	}
	par, perr := parseKV(costs[2], "p=")
	if perr != nil {
		err = ErrFormat
		return
	}

	salt, perr = base64.RawStdEncoding.DecodeString(parts[4])
	if perr != nil {
		err = ErrFormat
		return
	}
	hash, perr = base64.RawStdEncoding.DecodeString(parts[5])
	if perr != nil {
		err = ErrFormat
		return
	}

	memory = uint32(mem)
	iterations = uint32(iter)
	parallel = uint8(par)
	return
}

func parseKV(s, prefix string) (uint64, error) {
	if !strings.HasPrefix(s, prefix) {
		return 0, ErrFormat
	}
	return strconv.ParseUint(strings.TrimPrefix(s, prefix), 10, 32)
}
