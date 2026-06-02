// Package rlstest holds typed helpers for setting the Postgres
// session GUCs that RLS policies consult (`app.tenant_id`,
// `app.is_platform`). Tests use these helpers instead of issuing raw
// `SELECT set_config(...)` queries.
//
// Why: sqlc doesn't model Postgres session-level GUC setting. That's
// a legitimate reason for the original tests to use raw SQL — but
// the call shape (`pool.Exec(ctx, "SELECT set_config($1, $2, true)", name, id)`)
// is identical across 8+ files. Centralising in a helper:
//   - Closes the raw-SQL-in-tests gate (TestArch_NoRawSQLInTests)
//   - Documents the GUC scope (transaction-local vs. session-level)
//   - Makes the wire-name of each GUC discoverable
package rlstest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// GUC names re-export the single source of truth in package pg
// ([pg.GUCTenantID] / [pg.GUCIsPlatform]) so the test helpers and the
// production set_config callers can never desync. Bound as a SQL
// parameter to set_config — never spelled inline — so the literal lives
// in exactly one place.
const (
	GUCAppTenantID   = pg.GUCTenantID
	GUCAppIsPlatform = pg.GUCIsPlatform
)

// SetSessionTenant binds app.tenant_id on the connection for the
// remainder of the current TRANSACTION (`is_local=true`). Wraps
// `SELECT set_config('app.tenant_id', $1, true)`. Use inside a
// BEGIN/COMMIT block; the binding evaporates at COMMIT.
//
// For session-wide binding (rare in tests; used in
// long-lived fixture connections), use SetSessionTenantPersistent.
func SetSessionTenant(t testing.TB, ctx context.Context, tx pgx.Tx, tenantID string) {
	t.Helper()
	const q = `SELECT set_config($1, $2, true)`
	if _, err := tx.Exec(ctx, q, GUCAppTenantID, tenantID); err != nil {
		t.Fatalf("rlstest.SetSessionTenant(%s): %v", tenantID, err)
	}
}

// SetSessionTenantPersistent sets app.tenant_id session-wide
// (`is_local=false`). Used in EXPLAIN-gate fixtures where the
// pool-acquired connection lives across multiple queries that
// each need the same RLS scope.
func SetSessionTenantPersistent(t testing.TB, ctx context.Context, conn *pgx.Conn, tenantID string) {
	t.Helper()
	const q = `SELECT set_config($1, $2, false)`
	if _, err := conn.Exec(ctx, q, GUCAppTenantID, tenantID); err != nil {
		t.Fatalf("rlstest.SetSessionTenantPersistent(%s): %v", tenantID, err)
	}
}

// SetSessionPlatform binds app.is_platform=true session-wide
// (`is_local=false`). Used by adapter integration tests that need
// to bypass tenant-RLS for cross-tenant operations (audit reads,
// platform listings).
//
// Always pairs with [SetSessionTenant] in transactional contexts —
// platform scope still needs a tenant_id ANCHOR for the connection
// even when RLS bypasses tenant filtering.
func SetSessionPlatform(t testing.TB, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	const q = `SELECT set_config($1,'true',false)`
	if _, err := conn.Exec(ctx, q, GUCAppIsPlatform); err != nil {
		t.Fatalf("rlstest.SetSessionPlatform: %v", err)
	}
}

// SetSessionPlatformLocal binds app.is_platform=true for the
// remainder of the current TRANSACTION (`is_local=true`). Wraps
// `SELECT set_config('app.is_platform','true',true)`. Use inside
// a BEGIN/COMMIT block; the binding evaporates at COMMIT.
//
// Counterpart of [SetSessionPlatform] for EXPLAIN-gate fixtures
// that scope the platform GUC to a single read.
func SetSessionPlatformLocal(t testing.TB, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	const q = `SELECT set_config($1,'true',true)`
	if _, err := tx.Exec(ctx, q, GUCAppIsPlatform); err != nil {
		t.Fatalf("rlstest.SetSessionPlatformLocal: %v", err)
	}
}
