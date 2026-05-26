//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// identity/app/command integration test package.
//
// Mirrors the identity/adapters + identity/ports/subscribers shape:
// ONE Postgres testcontainer per package (pgtest.RunMain) + shared
// pgxpool consumed by every test via startWiredPostgres(t). Per-test
// isolation via fresh tenant_id + RLS.

package command_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool. Set
// by TestMain; consumed by every test via startWiredPostgres().
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstrap. Schemas + Grants reflect what the Register +
// Login + RotateRefresh flow tests touch: identity schema only.
//
// Wraps m.Run() with goleak after-test check; replaces the deleted
// testmain_integration_test.go.
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"identity"},
		Grants:  []string{"identity"},
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

// startWiredPostgres returns the shared package-scoped Postgres pool.
// Each caller gets the SAME pool — isolation between tests comes from
// fresh tenant_ids per test + RLS.
//
// Signature preserved from the previous per-test-container shape so
// existing tests don't need to change their call sites.
func startWiredPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}
