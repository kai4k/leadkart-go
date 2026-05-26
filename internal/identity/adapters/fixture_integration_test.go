//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// identity/adapters integration test package.
//
// PERFORMANCE SHAPE (Go+Postgres canon per Brandur Leach / TDL Wild
// Workouts / mattermost-server):
//
//   - ONE Postgres testcontainer per test PACKAGE, bootstrapped in
//     TestMain (~10s one-time cost).
//   - ONE shared pgxpool used by every test in the package (pgxpool is
//     goroutine-safe; multiplexes per-tx connections via Acquire).
//   - Per-test isolation via fresh tenant_id + RLS GUC binding —
//     every test that's tenant-scoped uses uuid.NewV7() for its
//     tenant.ID and binds it to ctx via tenancy.WithID(). RLS ensures
//     no other parallel test's rows are visible.
//   - t.Parallel() everywhere — Postgres connection pool + RLS
//     partitioning makes this safe.
//   - For tests that NEED cross-tenant isolation (outbox forwarder
//     reading all tenants, platform-scope reads): call
//     sharedPG.TruncateAll(t) at the top of the test + DON'T call
//     t.Parallel() in those.
//
// Previous shape (each test called repoFixture which spun a fresh
// container + reapplied 40+ migrations) took ~15s per test × 60+
// tests = ~15min just for adapters. After this refactor: ~30-60s
// total for the same suite.
//
// Per-file factories (newTenant, newRole, etc.) stay in the files
// that own them — this fixture only owns the shared infrastructure
// (TestMain, sharedPG, repoFixture).

package adapters_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool. Set
// by TestMain; consumed by every test via repoFixture / sharedPG.Pool().
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain is the package-scoped bootstrap. Spins ONE container,
// applies migrations, provisions the leadkart_app role with grants
// on the identity schema. Wraps m.Run() with goleak's after-test
// goroutine-leak check (was previously in testmain_integration_test.go;
// merged here so all bootstrap discipline lives in one file).
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

	// Goroutine-leak check happens AFTER container cleanup; testcontainers'
	// reaper goroutine + pgxpool's background health-check are ignored
	// because they're managed by their respective libraries' shutdowns
	// (already invoked by pgtest.RunMain's defer).
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

// repoFixture returns the shared package-scoped Postgres pool. Each
// caller gets the SAME pool — isolation between tests comes from
// fresh tenant_ids + RLS, NOT from per-test database state.
//
// Tests that NEED cross-tenant isolation should call
// sharedPG.TruncateAll(t) explicitly + opt out of t.Parallel().
//
// Function signature preserved from the previous per-test-container
// shape so existing tests don't need to change their call sites — just
// add t.Parallel() at the top.
func repoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}
