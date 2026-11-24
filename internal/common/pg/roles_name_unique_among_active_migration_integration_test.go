//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds an inactive and an active role that share a name, which no typed helper can produce.
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

const rolesNameUniqueAmongActive = 20261125000008

// TestMigration_RolesNameUniqueAmongActive_DownRenamesInactiveDuplicates: after the Up an inactive role
// may share its name with an active one. Restoring the full unique index on Down would fail, so Down
// renames each inactive duplicate deterministically (its id appended) and keeps the row. ADR 0073
// addendum 2026-11-25.
func TestMigration_RolesNameUniqueAmongActive_DownRenamesInactiveDuplicates(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, dir, rolesNameUniqueAmongActive); err != nil {
		t.Fatalf("up: %v", err)
	}
	at := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	tenantID := seedTenant(ctx, t, db, "role-names", at)
	role := func(name string, active bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_active)
			VALUES ($1, $2, $3, false, false, 60, '["tenant.admin"]'::jsonb, $4, $5)`, id, tenantID, name, at, active)
		return id
	}
	live, gone1, gone2 := role("Area Manager", true), role("Area Manager", false), role("Area Manager", false)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_active)
		VALUES ($1, $2, 'Area Manager', false, false, 60, '[]'::jsonb, $3, true)`, uuid.NewString(), tenantID, at); err == nil {
		t.Fatal("two ACTIVE roles with one name must violate the partial unique index")
	}

	if err := goose.DownToContext(ctx, db, dir, rolesNameUniqueAmongActive-1); err != nil {
		t.Fatalf("down over inactive duplicates: %v", err)
	}
	name := func(id string) string {
		var v string
		if err := db.QueryRowContext(ctx, `SELECT name FROM identity.roles WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := name(live); got != "Area Manager" {
		t.Errorf("the active role was renamed to %q", got)
	}
	for _, id := range []string{gone1, gone2} {
		if got := name(id); !strings.HasSuffix(got, "~"+id) {
			t.Errorf("inactive role %s is named %q, want its id appended", id, got)
		}
	}
	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
