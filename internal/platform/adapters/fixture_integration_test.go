//go:build integration

// TestMain + shared pool for the platform/adapters integration package.
//
// Shape (Brandur Leach / TDL Wild Workouts canon):
//   - ONE Postgres testcontainer per package, bootstrapped in TestMain.
//   - ONE shared pgxpool (goroutine-safe; per-tx conns via Acquire).
//   - Per-test isolation via fresh tenant_id + RLS for tenant-scoped tests;
//     cross-tenant tests call sharedPG.TruncateAll(t) and opt out of
//     t.Parallel().

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

// sharedPG is the per-package container + app-role pool, set by TestMain.
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstraps one container, applies migrations, provisions the
// leadkart_app role with grants on the platform + identity schemas, then
// wraps m.Run() with goleak's leak check.
func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		// USAGE on these schemas ("app" implicit).
		Schemas: []string{"identity", "platform"},
		// DML on these.
		Grants: []string{"identity", "platform"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})

	// Leak check runs after container cleanup; testcontainers' reaper and
	// pgxpool's health-check goroutine are owned by their libraries' shutdowns
	// (already invoked by pgtest.RunMain's defer).
	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// platformPool returns the shared package-scoped pool; isolation comes from
// fresh tenant_ids + RLS, not per-test database state. Cross-tenant tests
// must call sharedPG.TruncateAll(t) and opt out of t.Parallel().
func platformPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// openRawDB returns a database/sql handle on the pool's DSN, for verification
// queries that bypass the repository (e.g. outbox-row inspection).
func openRawDB(t *testing.T, pool *pgxpool.Pool) (*sql.DB, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig
	dsn := cfg.ConnString()
	if dsn == "" {
		return nil, errors.New("pool has no ConnString")
	}
	return sql.Open("pgx", dsn)
}

// fixtureForm returns a valid leadform.Form for seeding.
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

// nowUTC is a deterministic pinned timestamp for tests.
func nowUTC() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UTC()
}
