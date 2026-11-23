//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds a ledger row of the type the migration under test adds, which no typed helper can produce.
package pg_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const returnToMakerMigration = 20261124000004

// TestMigration_ReturnToMakerDownWorksOnData: once a return_to_maker row exists the Down must
// still run (review L3) and it must not delete a ledger row (ADR 0071). Up again is clean.
func TestMigration_ReturnToMakerDownWorksOnData(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, dir, returnToMakerMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	tenantID := seedTenant(ctx, t, db, "return-to-maker", at)
	productID, batchID := uuid.NewString(), uuid.NewString()
	exec(ctx, t, db, `INSERT INTO inventory.products (id, tenant_id, sku, name, dosage_form, pack_size, hsn_code, gst_rate_bps, created_at, updated_at)
		VALUES ($1, $2, 'RTM-1', 'Return drug', 'Tablet', '10', '3004', 1200, $3, $3)`, productID, tenantID, at)
	exec(ctx, t, db, `INSERT INTO inventory.batches (id, product_id, tenant_id, batch_number, manufacture_date, expiry_date, manufacturer_name,
		manufacturing_licence_number, mrp_paise, purchase_price_paise, quantity_on_hand, created_at, updated_at)
		VALUES ($1, $2, $3, 'LOT-1', '2025-01-01', '2026-10-31', 'Acme', 'ML-1', 100, 50, 0, $4, $4)`, batchID, productID, tenantID, at)
	exec(ctx, t, db, `INSERT INTO inventory.stock_movements (id, batch_id, product_id, tenant_id, type, quantity, quantity_on_hand_after, reason, actor_membership_id, occurred_at)
		VALUES ($1, $2, $3, $4, 'return_to_maker', -5, 0, 'sent back to the maker', $5, $6)`, uuid.NewString(), batchID, productID, tenantID, uuid.NewString(), at)

	if err := goose.DownToContext(ctx, db, dir, returnToMakerMigration-1); err != nil {
		t.Fatalf("down on data: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM inventory.stock_movements WHERE type = 'return_to_maker'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ledger rows of the type after down = %d, %v; want 1 (nothing is deleted)", n, err)
	}
	if err := goose.UpToContext(ctx, db, dir, returnToMakerMigration); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
