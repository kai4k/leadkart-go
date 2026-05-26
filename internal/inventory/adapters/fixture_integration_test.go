//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// inventory/adapters integration test package.
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
// Per-file factories (newTenant, seedTenant, tenantCtx, openRawDB,
// fixedNow/testNow) stay in this file — they're inventory-specific
// helpers, not shared infrastructure.

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

// sharedPG is the per-package Postgres container + app-role pool. Set
// by TestMain; consumed by every test via repoFixture / sharedPG.Pool().
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// fixedNow is the deterministic timestamp inventory integration tests
// pass to domain factories and mutators. Replaces the prior
// package-global clock per the clock-injection refactor — each test
// supplies the instant explicitly so no two parallel test files can
// race on a shared mutable clock.
//
// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// testNow is an alias for fixedNow, used by aggregate factories that
// the shared rewrite script standardised on testNow.
//
// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var testNow = fixedNow

// TestMain is the package-scoped bootstrap. Spins ONE container,
// applies migrations, provisions the leadkart_app role with grants
// on the inventory + identity schemas. Wraps m.Run() with goleak's
// after-test goroutine-leak check (was previously in
// testmain_integration_test.go; merged here so all bootstrap
// discipline lives in one file).
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		// USAGE on these schemas. "app" is implicit.
		Schemas: []string{"identity", "inventory"},
		// DML on these. identity is included because the inventory tests
		// seed real tenant rows through the identity TenantRepository to
		// satisfy the composite FK on inventory.products.
		Grants: []string{"identity", "inventory"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

	// Goroutine-leak check happens AFTER container cleanup;
	// testcontainers' reaper goroutine + pgxpool's background
	// health-check are ignored because they're managed by their
	// respective libraries' shutdowns (already invoked by
	// pgtest.RunMain's defer).
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

// seedTenant inserts a fresh tenant row via the identity
// TenantRepository so inventory tests have a real tenant id
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

// tenantCtx binds the tenant_id GUC for tenant-scoped repo calls.
func tenantCtx(t *testing.T, id tenant.ID) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(id.String()))
}

// openRawDB returns a database/sql handle pointed at the same DSN as
// the pool, useful for SELECTs that need to bypass the inventory
// adapter + flip platform-bypass for outbox checks.
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database)
	return sql.Open("pgx", dsn)
}
