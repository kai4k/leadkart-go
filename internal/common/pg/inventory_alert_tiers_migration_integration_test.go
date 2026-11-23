//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds rows under the schema of the migration under test (two tiers of one batch on one date), which no typed helper can produce.
package pg_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const alertEmissionTiersMigration = 20261124000002

// TestMigration_AlertEmissionTiersDownWorksOnData: one batch can raise two tiers on one
// date; Down must still restore the old (tenant, kind, subject, date) key (review S10).
func TestMigration_AlertEmissionTiersDownWorksOnData(t *testing.T) {
	dsn := startPostgres(t)
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	dir := migrationsDir(t)
	if err := goose.UpToContext(ctx, db, dir, alertEmissionTiersMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	tenantID := seedTenant(ctx, t, db, "alert-tiers", at)
	subject := uuid.NewString()
	for _, row := range []struct{ kind, key string }{
		{"batch_expiring", "tier:90:2027-02-28"},
		{"batch_expiring", "tier:60:2027-02-28"},
		{"expired_on_hand", "once"},
	} {
		exec(ctx, t, db, `INSERT INTO inventory.alert_emissions (tenant_id, kind, subject_id, emitted_date, emission_key) VALUES ($1, $2, $3, $4, $5)`,
			tenantID, row.kind, subject, at, row.key)
	}
	if err := goose.DownToContext(ctx, db, dir, alertEmissionTiersMigration-1); err != nil {
		t.Fatalf("down on data: %v", err)
	}
	if n := countAlertEmissions(ctx, t, db); n != 1 {
		t.Errorf("rows after down = %d, want 1 (two tiers of one date collapse, expired_on_hand dropped)", n)
	}
}

func countAlertEmissions(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM inventory.alert_emissions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
