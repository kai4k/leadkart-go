//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// tasks/adapters integration test package. Mirror of
// internal/crm/adapters/fixture_integration_test.go.

package adapters_test

import (
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool.
//
//nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain is the package-scoped bootstrap. Spins ONE container,
// applies migrations, provisions the leadkart_app role with grants
// on the tasks + identity schemas (identity grants needed for the
// hierarchy traversal bridge in cmd/api).
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"identity", "tasks"},
		Grants:  []string{"identity", "tasks"},
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
