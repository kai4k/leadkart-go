// Package ids generates UUIDv7 identifiers and owns the ONE canonical form
// every untrusted identifier is reduced to before it is compared or stored.
//
// UUIDv7 (RFC 9562) puts a Unix-millisecond timestamp in the leading bits,
// giving B-tree-friendly time-ordering for primary keys (matters at scale —
// random UUIDv4 keys cause heavy index bloat in Postgres).
//
// All bounded-context typed IDs (TenantID, PersonID, MembershipID, ...) wrap
// `uuid.UUID` returned from this package. Per-domain wrappers stay in their
// own packages; this package owns generation and [Canonical].
package ids

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
)

// ErrInvalidID is returned by [Canonical] for anything that is not exactly one
// non-nil UUID. Domain ParseID wrappers re-wrap it with their own sentinel.
var ErrInvalidID = errs.New(errs.KindInvalidInput, "ids", "invalid id")

// Canonical is the single canonical-id function (ADR 0069, finding 1): every
// layer that turns an untrusted id into a comparable value goes through it, so
// two spellings of one id can never be treated as two ids.
//
// It accepts the spellings uuid.Parse accepts (upper case, "urn:uuid:" prefix,
// "{...}" braces, hyphenless 32-hex) and returns the lowercase hyphenated form.
// It REJECTS the empty string, the nil UUID, and any leading, trailing or
// internal whitespace (no TrimSpace: a leading space, a trailing newline or NBSP+id is not an id).
func Canonical(raw string) (string, error) {
	// uuid.Parse drops the first and last byte of any 38-character input
	// without checking they are braces, so " <uuid> " would parse. The braced
	// form is the only valid 38-character spelling.
	if len(raw) == 38 && (raw[0] != '{' || raw[37] != '}') {
		return "", ErrInvalidID
	}
	u, err := uuid.Parse(raw)
	if err != nil || u == uuid.Nil {
		return "", ErrInvalidID
	}
	return u.String(), nil
}

// NewV7 returns a fresh UUIDv7.
//
// Panics if the underlying crypto random source is unavailable. That's a
// process-fatal condition (no fallback meaningful at request time), so we
// surface it as a panic the supervisor can restart from.
func NewV7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		// crypto/rand failure is process-fatal in any practical Go runtime;
		// the supervisor should restart. No request-level recovery exists.
		panic(fmt.Errorf("ids: NewV7: %w", err))
	}
	return id
}
