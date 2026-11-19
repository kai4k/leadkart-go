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

	"github.com/leadkart/leadkart-go/internal/common/pg/rlstest"
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

// SetPermissionRequestPermission rewrites the permission name stored on one
// permission-request row, in a platform-scoped transaction (the application
// role is bound by row-level security). It exists to put a row into the shape
// older code stored, so tests can prove the current code refuses to act on
// it. Exactly one row must change.
func SetPermissionRequestPermission(t testing.TB, pool *pgxpool.Pool, requestID, permission string) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("identitytest.SetPermissionRequestPermission: begin: %v", err)
	}
	defer tx.Rollback(t.Context()) //nolint:errcheck // rollback after commit is a no-op
	rlstest.SetSessionPlatformLocal(t, t.Context(), tx)
	const q = `UPDATE identity.permission_requests SET permission_constant = $1 WHERE id = $2`
	tag, err := tx.Exec(t.Context(), q, permission, requestID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("identitytest.SetPermissionRequestPermission(%s): rows %d err %v, want exactly 1 row", requestID, tag.RowsAffected(), err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("identitytest.SetPermissionRequestPermission: commit: %v", err)
	}
}
