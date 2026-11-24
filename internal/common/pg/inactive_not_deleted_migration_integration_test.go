//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds soft-deleted rows that share a SKU or a role name with live ones, which no typed helper can produce.
package pg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const beforeInactiveNotDeleted = 20261125000004

// TestMigration_InactiveNotDeletedKeepsDeletedDuplicates: the old unique indexes covered live rows
// only, so a soft-deleted product or role could share its SKU or name with a live one (or with
// another deleted one). The new indexes cover every row, so the Up renames each such deleted row
// deterministically instead of failing, keeps it (inactive, never active again) and leaves live
// rows alone. ADR 0073.
func TestMigration_InactiveNotDeletedKeepsDeletedDuplicates(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, dir, beforeInactiveNotDeleted); err != nil {
		t.Fatalf("up to the previous migration: %v", err)
	}
	at := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	tenantID := seedTenant(ctx, t, db, "dup-names", at)

	role := func(name string, deleted bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_deleted, deleted_at)
			VALUES ($1, $2, $3, false, false, 60, '["tenant.admin"]'::jsonb, $4, $5, CASE WHEN $5 THEN $4::timestamptz END)`, id, tenantID, name, at, deleted)
		return id
	}
	product := func(sku string, deleted bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO inventory.products (id, tenant_id, sku, name, dosage_form, pack_size, hsn_code, gst_rate_bps, created_at, updated_at, is_deleted, deleted_at)
			VALUES ($1, $2, $3, 'Product', 'Tablet', '10', '3004', 1200, $4, $4, $5, CASE WHEN $5 THEN $4::timestamptz END)`, id, tenantID, sku, at, deleted)
		return id
	}
	batch := func(productID, number string, deleted bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO inventory.batches (id, product_id, tenant_id, batch_number, manufacture_date, expiry_date, manufacturer_name,
			manufacturing_licence_number, mrp_paise, purchase_price_paise, quantity_on_hand, created_at, updated_at, is_deleted, deleted_at)
			VALUES ($1, $2, $3, $4, '2025-01-01', '2026-10-31', 'Acme', 'ML-1', 100, 50, 0, $5, $5, $6, CASE WHEN $6 THEN $5::timestamptz END)`, id, productID, tenantID, number, at, deleted)
		return id
	}
	liveRole, goneRole := role("Sales", false), role("Sales", true)
	oldA, oldB := role("Old", true), role("Old", true)
	liveSKU, goneSKU := product("DUP-1", false), product("DUP-1", true)
	delA, delB := product("DEL-2", true), product("DEL-2", true)

	batchProduct := product("BATCH-OWNER", false)
	liveBatch, goneBatch := batch(batchProduct, "LOT-1", false), batch(batchProduct, "LOT-1", true)

	// Max-length values: the rename keeps the id suffix whole and trims the PREFIX, so it stays
	// within the column limit and two rows that differ only past the trimmed part still get
	// different names (the ids differ).
	longRole, longSKU, longLot := strings.Repeat("r", 100), strings.Repeat("s", 64), strings.Repeat("l", 100)
	liveLongRole, goneLongRole := role(longRole, false), role(longRole, true)
	goneLongRole2 := role(longRole, true)
	liveLongSKU, goneLongSKU, goneLongSKU2 := product(longSKU, false), product(longSKU, true), product(longSKU, true)
	liveLongLot, goneLongLot := batch(batchProduct, longLot, false), batch(batchProduct, longLot, true)

	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("up over soft-deleted duplicates: %v", err)
	}

	str := func(q, id string) string {
		var v string
		if err := db.QueryRowContext(ctx, q, id).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	if got := str(`SELECT name FROM identity.roles WHERE id = $1`, liveRole); got != "Sales" {
		t.Errorf("the live role was renamed to %q", got)
	}
	for _, id := range []string{goneRole, oldA, oldB} {
		if got := str(`SELECT name FROM identity.roles WHERE id = $1`, id); !strings.HasSuffix(got, "~"+id) {
			t.Errorf("deleted role %s is named %q, want its id appended", id, got)
		}
		if got := str(`SELECT is_active::text FROM identity.roles WHERE id = $1`, id); got != "false" {
			t.Errorf("deleted role %s is_active = %s, want false (inactive, never active again)", id, got)
		}
	}
	if got := str(`SELECT sku FROM inventory.products WHERE id = $1`, liveSKU); got != "DUP-1" {
		t.Errorf("the live product SKU became %q", got)
	}
	for _, id := range []string{goneSKU, delA, delB} {
		if got := str(`SELECT sku FROM inventory.products WHERE id = $1`, id); !strings.HasSuffix(got, "~"+id) || len(got) > 64 {
			t.Errorf("deleted product %s has SKU %q, want its id appended within 64 characters", id, got)
		}
		if got := str(`SELECT is_active::text FROM inventory.products WHERE id = $1`, id); got != "false" {
			t.Errorf("deleted product %s is_active = %s, want false", id, got)
		}
	}
	// Batches (ADR 0073): the same, and a batch that was deleted is visible again (never hidden).
	if got := str(`SELECT batch_number FROM inventory.batches WHERE id = $1`, liveBatch); got != "LOT-1" {
		t.Errorf("the live batch number became %q", got)
	}
	if got := str(`SELECT batch_number FROM inventory.batches WHERE id = $1`, goneBatch); !strings.HasSuffix(got, "~"+goneBatch) {
		t.Errorf("deleted batch %s is numbered %q, want its id appended", goneBatch, got)
	}
	for _, c := range []struct {
		table, col, live string
		gone             []string
		max              int
		liveWant         string
	}{
		{"identity.roles", "name", liveLongRole, []string{goneLongRole, goneLongRole2}, 100, longRole},
		{"inventory.products", "sku", liveLongSKU, []string{goneLongSKU, goneLongSKU2}, 64, longSKU},
		{"inventory.batches", "batch_number", liveLongLot, []string{goneLongLot}, 100, longLot},
	} {
		if got := str(`SELECT `+c.col+` FROM `+c.table+` WHERE id = $1`, c.live); got != c.liveWant {
			t.Errorf("%s: the live max-length value changed", c.table)
		}
		seen := map[string]bool{}
		for _, id := range c.gone {
			got := str(`SELECT `+c.col+` FROM `+c.table+` WHERE id = $1`, id)
			if !strings.HasSuffix(got, "~"+id) || len(got) > c.max || seen[got] {
				t.Errorf("%s: deleted max-length row %s is %q (len %d): want the whole id suffix, within %d, unique", c.table, id, got, len(got), c.max)
			}
			seen[got] = true
		}
	}
}
