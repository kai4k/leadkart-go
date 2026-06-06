//go:build integration

// TestMain + shared pool for the orders/adapters integration package.
// One Postgres testcontainer + one shared pgxpool; per-test isolation via
// fresh tenant_id + RLS. Mirrors the dispatch/crm fixture shape.

package adapters_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package container + app-role pool, set by TestMain.
//
//nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstraps one container, applies all migrations, provisions the
// leadkart_app role with grants on the orders schema, then wraps m.Run() with
// goleak's leak check.
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"orders"},
		Grants:  []string{"orders"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// ordersPool returns the shared package-scoped pool; isolation comes from
// fresh tenant_ids + RLS, not per-test database state.
func ordersPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// nowUTC is a deterministic pinned timestamp for tests.
func nowUTC() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UTC()
}
