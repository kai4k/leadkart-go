//go:build integration

package pg_test

import (
	"context"
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
	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
