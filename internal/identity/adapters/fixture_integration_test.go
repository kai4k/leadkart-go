//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// identity/adapters integration test package.
//
// ONE Postgres testcontainer per package bootstrapped in TestMain; ONE
// shared pgxpool reused by all tests. Per-test isolation via a fresh
// tenant_id + RLS GUC binding (t.Parallel() safe). Tests that require
// cross-tenant visibility call sharedPG.TruncateAll(t) and omit
// t.Parallel(). Per-file factories (newTenant, newRole, etc.) live in
// their own files; this file owns only the shared infrastructure.

package adapters_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container set by TestMain.
//
//nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstraps the shared Postgres container, applies migrations,
// and checks for goroutine leaks after m.Run().
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		// USAGE on these schemas. "app" is implicit.
		Schemas: []string{"identity", "inventory"},
		// DML on these. inventory is included because a handful of
		// older tests cross the FK boundary (legacy from when identity
		// and inventory shared a migrations bundle).
		Grants: []string{"identity", "inventory"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

	// Goroutine-leak check runs after container cleanup; library-managed
	// goroutines are filtered via pgtest.GoleakOptions().
	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// repoFixture returns the shared package-scoped pool. All callers share
// the same pool; isolation is via fresh tenant_ids + RLS. Tests needing
// cross-tenant isolation must call sharedPG.TruncateAll(t) and omit
// t.Parallel().
func repoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}
