// Package identitytest holds typed read-side helpers for integration
// tests that need to assert on identity-schema state without each
// caller re-deriving the same raw SQL.
//
// Why this package exists: see audittest + messagingtest — the
// broader rationale is "tests get the same typed-helper discipline
// as production (sqlc + adapters); no raw SQL in test files".
//
// The companion arch test TestArch_NoRawSQLInTests enforces it.
package identitytest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GetSecurityStamp returns the live security_stamp value stored on
// the identity.persons row for the supplied personID. Used by E2E
// fixtures that mint synthetic JWTs needing the freshness-check
// middleware to accept them.
//
// Failure (query error) calls t.Fatalf — callers should treat the
// returned stamp as authoritative.
func GetSecurityStamp(t testing.TB, pool *pgxpool.Pool, personID string) string {
	t.Helper()
	var stamp string
	const q = `SELECT security_stamp::text FROM identity.persons WHERE id = $1`
	if err := pool.QueryRow(t.Context(), q, personID).Scan(&stamp); err != nil {
		t.Fatalf("identitytest.GetSecurityStamp(%s): %v", personID, err)
	}
	return stamp
}
