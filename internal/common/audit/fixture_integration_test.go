//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// common/audit integration tests.
//
// Audit-log purge tests do NOT use t.Parallel because they assert on
// global row counts (audit_log_entry isn't tenant-scoped — it's a
// platform-wide append-only log). Each test calls
// sharedPG.TruncateAll(t) at the top so it starts from a clean slate.
// This puts them in Phase 1 (serial) per Go's two-phase scheduling.

package audit_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool.
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstrap. audit-log writes go through buildingblocks
// schema; identity schema is included for the FK from audit_log_entry.
func TestMain(m *testing.M) {
	c, code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"identity", "buildingblocks"},
		Grants:  []string{"identity", "buildingblocks"},
	})
	sharedPG = c

	if err := goleak.Find(
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// startPostgres returns the shared package-scoped Postgres pool.
// Function signature preserved so existing tests don't change call sites.
func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}
