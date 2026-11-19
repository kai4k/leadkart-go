//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds the PRE-migration role rows (old seed levels and permission sets) that no current typed helper can produce, then asserts the post-Up and post-Down rows.
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

const roleTiersMigration = 20261119000001

const oldSalesManagerPerms = `["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]`

func TestMigration_IdentityRoleTiers_UpgradesPristineSeedsOnly(t *testing.T) {
	dsn := startPostgres(t)
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}
	batch := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pristine, custom := seedTenant(ctx, t, db, "tiers-a", batch), seedTenant(ctx, t, db, "tiers-b", batch)
	insertRole(ctx, t, db, pristine, "Sales Manager", 50, oldSalesManagerPerms, batch)
	insertRole(ctx, t, db, custom, "Sales Manager", 40, oldSalesManagerPerms, batch)       // level customised
	insertRole(ctx, t, db, custom, "HR Manager", 50, `["tasks.work_items.read"]`, batch)   // perms customised
	insertRole(ctx, t, db, custom, "Purchase Manager", 50, `[]`, batch.Add(time.Hour))     // tenant-created homonym
	insertRole(ctx, t, db, custom, "Legacy Level Zero", 0, `["crm.leads.read"]`, batch)    // custom role stored at level 0
	insertRole(ctx, t, db, pristine, "Legacy Level Zero", 0, `[]`, batch.Add(time.Minute)) // same, other tenant
	insertDeletedRole(ctx, t, db, custom, "Gone Level Zero", 0, batch)                     // soft-deleted level-0 role

	if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertRole(ctx, t, db, pristine, "Sales Manager", 20, "crm.leads.read_all", true)
	assertRole(ctx, t, db, custom, "Sales Manager", 40, "crm.leads.read_all", true) // perms still pristine
	assertRole(ctx, t, db, custom, "HR Manager", 20, "identity.roles.approve", false)
	assertRole(ctx, t, db, custom, "Purchase Manager", 50, "identity.roles.approve", false)
	assertRole(ctx, t, db, pristine, "Team Lead", 30, "tasks.work_items.reassign", true)
	assertRole(ctx, t, db, pristine, "Company Owner", 0, "tenant.admin", true)

	// A custom level-0 role is moved to 1; the CompanyOwner role is untouched.
	assertRole(ctx, t, db, custom, "Legacy Level Zero", 1, "crm.leads.read", true)
	assertRole(ctx, t, db, pristine, "Legacy Level Zero", 1, "crm.leads.read", false)
	assertRole(ctx, t, db, custom, "Company Owner", 0, "tenant.admin", true)
	var zeros int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity.roles WHERE hierarchy_level = 0 AND NOT (name = 'Company Owner' AND is_system_default)`).Scan(&zeros); err != nil || zeros != 0 {
		t.Fatalf("roles left at level 0 besides CompanyOwner: n=%d err=%v", zeros, err)
	}
	var goneLevel int
	if err := db.QueryRowContext(ctx, `SELECT hierarchy_level FROM identity.roles WHERE tenant_id = $1 AND name = 'Gone Level Zero'`, custom).Scan(&goneLevel); err != nil || goneLevel != 1 {
		t.Fatalf("soft-deleted level-0 role: level=%d err=%v, want 1", goneLevel, err)
	}

	if err := goose.DownToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertRole(ctx, t, db, pristine, "Sales Manager", 50, "crm.leads.read_all", false)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity.roles WHERE tenant_id = $1 AND name = 'Team Lead'`, pristine).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Team Lead after down: n=%d err=%v", n, err)
	}
	// Down cannot restore the old level of the re-levelled roles.
	assertRole(ctx, t, db, custom, "Legacy Level Zero", 1, "crm.leads.read", true)
}

func seedTenant(ctx context.Context, t *testing.T, db *sql.DB, slug string, batch time.Time) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at, activated_at)
		VALUES ($1, $2, $3, $3, 'active', $4, $4)`, id, slug, slug+" Pvt Ltd", batch); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
		VALUES ($1, $2, 'Company Owner', true, false, 0, '["tenant.admin"]'::jsonb, $3)`, uuid.NewString(), id, batch); err != nil {
		t.Fatalf("seed owner role: %v", err)
	}
	return id
}

func insertRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, level int, perms string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
		VALUES ($1, $2, $3, false, false, $4, $5::jsonb, $6)`, uuid.NewString(), tenantID, name, level, perms, at); err != nil {
		t.Fatalf("insert role %s: %v", name, err)
	}
}

func insertDeletedRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, level int, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_deleted, deleted_at)
		VALUES ($1, $2, $3, false, false, $4, '[]'::jsonb, $5, true, $5)`, uuid.NewString(), tenantID, name, level, at); err != nil {
		t.Fatalf("insert deleted role %s: %v", name, err)
	}
}

func assertRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, wantLevel int, perm string, wantPerm bool) {
	t.Helper()
	var level int
	var has bool
	if err := db.QueryRowContext(ctx, `SELECT hierarchy_level, permissions ? $3 FROM identity.roles
		WHERE tenant_id = $1 AND name = $2 AND NOT is_deleted`, tenantID, name, perm).Scan(&level, &has); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if level != wantLevel || has != wantPerm {
		t.Fatalf("%s: level=%d has(%s)=%v, want %d/%v", name, level, perm, has, wantLevel, wantPerm)
	}
}
