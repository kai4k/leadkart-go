//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// identity/ports/subscribers integration test package.
//
// Mirrors internal/identity/adapters/fixture_integration_test.go shape:
// ONE Postgres testcontainer per package (pgtest.RunMain) + shared
// pgxpool consumed by every test via newFixture(t). Per-test isolation
// via fresh tenant_id + RLS; miniredis + cache facade still spin
// per-test because they're cheap (~10ms vs ~15s for a fresh Postgres).
//
// Previous shape (newFixture spun a fresh Postgres per test) took
// ~15s × 8 tests = ~2-3min. After this refactor: ~10s container boot
// + ~50ms per test.

package subscribers_test

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool. Set
// by TestMain; consumed by every test via newFixture().
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstrap. Schemas + Grants reflect the subscriber tests'
// touch surface:
//   - identity:         tenants/persons/refresh_token_families tables
//   - buildingblocks:   audit_log (cascade subscriber writes audit rows)
//   - app:              outbox tables (forwarder + subscriber pre-flight)
//
// Wraps m.Run() with goleak after-test check; replaces the deleted
// testmain_integration_test.go.
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"identity", "buildingblocks"},
		Grants:  []string{"identity", "buildingblocks", "app"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

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
