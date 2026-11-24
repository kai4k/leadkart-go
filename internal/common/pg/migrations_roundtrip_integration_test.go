//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// TestMigrations_UpDownToZeroUp: every migration's Down runs, newest first, down to an empty
// database, and the whole set applies again. The Down of crm_reminders used to drop the
// constraint its own table's foreign key depends on before dropping the table.
func TestMigrations_UpDownToZeroUp(t *testing.T) {
	dsn := startPostgres(t)
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.EnsureGooseDialect(); err != nil {
		t.Fatalf("dialect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	dir := migrationsDir(t)
	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := goose.DownToContext(ctx, db, dir, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	assertNoApplicationObjects(ctx, t, db)
	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// assertNoApplicationObjects fails when anything a migration creates is left after down to 0:
// a relation (table, view, sequence, index), a type or a function in a schema that is not a
// system schema. goose's own version table and objects an extension owns are ignored.
func assertNoApplicationObjects(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		WITH app AS (
			SELECT oid, nspname FROM pg_namespace
			WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
		)
		SELECT 'relation', app.nspname || '.' || c.relname
		FROM pg_class c JOIN app ON app.oid = c.relnamespace
		WHERE c.relname NOT LIKE 'goose\_db\_version%'
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype IN ('e','i') AND d.classid = 'pg_class'::regclass)
		UNION ALL
		SELECT 'type', app.nspname || '.' || ty.typname
		FROM pg_type ty JOIN app ON app.oid = ty.typnamespace
		WHERE ty.typtype IN ('e','d','r','m')
		   OR (ty.typtype = 'c' AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype = ty.oid))
		UNION ALL
		SELECT 'function', app.nspname || '.' || p.proname
		FROM pg_proc p JOIN app ON app.oid = p.pronamespace
		WHERE NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("list leftovers: %v", err)
	}
	defer rows.Close()
	var left []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		left = append(left, kind+" "+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(left) > 0 {
		t.Fatalf("down to 0 left %d application object(s) behind: %v", len(left), left)
	}
}
