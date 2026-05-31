//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// inventory/adapters integration package.
//
// One Postgres testcontainer per package (Brandur Leach / TDL Wild
// Workouts canon). Tests share the pool; isolation comes from a fresh
// tenant_id + RLS per test — not a per-test container. Tests that need
// cross-tenant isolation (e.g. outbox forwarder) call
// sharedPG.TruncateAll(t) and skip t.Parallel().

package adapters_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgtest"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	identityadapters "github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// sharedPG is the per-package Postgres container and app-role pool, set
// by TestMain.
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// fixedNow is the deterministic timestamp passed to domain factories.
// Explicit per-test clock avoids races on a shared mutable clock.
//
// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// testNow aliases fixedNow for factories standardised on the testNow name.
//
// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var testNow = fixedNow

// TestMain bootstraps one Postgres container, applies migrations, and
// provisions grants on the inventory + identity schemas. Goleak runs
// after m.Run() to catch goroutine leaks.
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		// USAGE on these schemas. "app" is implicit.
		Schemas: []string{"identity", "inventory"},
		// identity included: inventory tests seed real tenant rows via
		// identity.TenantRepository to satisfy the composite FK.
		Grants: []string{"identity", "inventory"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

	// Leak check after container cleanup; library-owned goroutines
	// (testcontainers reaper, pgxpool health-check) are filtered by
	// pgtest.GoleakOptions.
	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// repoFixture returns the shared Postgres pool. Test isolation comes
// from fresh tenant IDs + RLS, not per-test DB state. Tests needing
// cross-tenant isolation must call sharedPG.TruncateAll(t) and skip
// t.Parallel().
func repoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// seedTenant inserts a tenant row via identity.TenantRepository,
// satisfying the composite FK on inventory.products.
func seedTenant(t *testing.T, pool *pgxpool.Pool) tenant.ID {
	t.Helper()
	tx := pg.NewTransactor(pool)
	repo := identityadapters.NewTenantRepository(pool, tx)
	id := tenant.ID(ids.NewV7().String())
	full := ids.NewV7().String()
	s, err := slug.New("inv-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@inv.test")
	tn, err := tenant.New(id, s, "Inv Pharma Pvt Ltd", "Inv", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return id
}

// tenantCtx returns a context with the tenant_id GUC bound.
func tenantCtx(t *testing.T, id tenant.ID) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(id.String()))
}

// openRawDB returns a database/sql handle on the same DSN as the pool,
// useful for queries that need to bypass the inventory adapter.
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	return sql.Open("pgx", dsn)
}
