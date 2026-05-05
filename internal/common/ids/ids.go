// Package ids generates UUIDv7 identifiers.
//
// UUIDv7 (RFC 9562) puts a Unix-millisecond timestamp in the leading bits,
// giving B-tree-friendly time-ordering for primary keys (matters at scale —
// random UUIDv4 keys cause heavy index bloat in Postgres).
//
// All bounded-context typed IDs (TenantID, PersonID, MembershipID, ...) wrap
// `uuid.UUID` returned from this package. Per-domain wrappers stay in their
// own packages; this package owns generation only.
package ids

import (
	"fmt"

	"github.com/google/uuid"
)

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
