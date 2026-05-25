//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// platform/adapters integration test package.
//
// PERFORMANCE SHAPE (Go+Postgres canon per Brandur Leach / TDL Wild
// Workouts / mattermost-server):
//
//   - ONE Postgres testcontainer per test PACKAGE, bootstrapped in
//     TestMain (~10s one-time cost).
//   - ONE shared pgxpool used by every test in the package (pgxpool is
//     goroutine-safe; multiplexes per-tx connections via Acquire).
//   - Per-test isolation via fresh tenant_id + RLS GUC binding for
//     tenant-scoped tests; cross-tenant tests (PlatformLead browse,
//     outbox global reads, EXPLAIN over the whole table) call
//     sharedPG.TruncateAll(t) at the top + opt out of t.Parallel().
//   - t.Parallel() everywhere it's safe — Postgres connection pool +
//     RLS partitioning makes this safe for tenant-scoped reads.
//
// Per-file factories (fixtureForm, nowUTC, openRawDB) stay in this
// file — they're platform-specific helpers, not shared infrastructure.

package adapters_test

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
)

// sharedPG is the per-package Postgres container + app-role pool. Set
// by TestMain; consumed by every test via platformPool / sharedPG.Pool().
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain is the package-scoped bootstrap. Spins ONE container,
// applies migrations, provisions the leadkart_app role with grants
// on the platform + identity schemas. Wraps m.Run() with goleak's
// after-test goroutine-leak check (was previously in
// testmain_integration_test.go; merged here so all bootstrap
// discipline lives in one file).
func TestMain(m *testing.M) {
	c, code := pgtest.RunMain(m, pgtest.Config{
		// USAGE on these schemas. "app" is implicit.
		Schemas: []string{"identity", "platform"},
		// DML on these.
		Grants: []string{"identity", "platform"},
	})
	sharedPG = c

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

// platformPool returns the shared package-scoped Postgres pool. Each
// caller gets the SAME pool — isolation between tests comes from
// fresh tenant_ids + RLS, NOT from per-test database state.
//
// Tests that NEED cross-tenant isolation (cross-tenant browse, global
// outbox reads, full-table EXPLAIN) should call sharedPG.TruncateAll(t)
// explicitly + opt out of t.Parallel().
//
// Function signature preserved from the previous per-test-container
// shape so existing tests don't need to change their call sites — just
// add t.Parallel() (or TruncateAll) at the top.
func platformPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// openRawDB returns a database/sql handle pointed at the same DSN as
// the pgxpool — used for verification queries that bypass the
// repository (e.g. outbox-row inspection under platform GUC).
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := cfg.ConnString()
	if dsn == "" {
		return nil, errors.New("pool has no ConnString")
	}
	return sql.Open("pgx", dsn)
}

// fixtureForm returns a valid leadform.Form for integration-test seed.
func fixtureForm(t *testing.T) leadform.Form {
	t.Helper()
	f, err := leadform.New(leadform.Input{
		ContactName:    "Acme Pharma Integration",
		MobileE164:     "+919876543210",
		Email:          "ops@acme.test",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		HasDrugLicence: true,
		HasGst:         true,
		GstNumber:      "27AAAAA0000A1Z5",
		HasPan:         true,
		PanNumber:      "AAAAA0000A",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		ProductRanges:  []string{"Antibiotics"},
		DosageForms:    []string{"Tablet"},
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	})
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	return f
}

// nowUTC is a deterministic, pinned timestamp for platform integration
// tests. Replaces the prior package-global clock per the
// clock-injection refactor.
func nowUTC() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UTC()
}
