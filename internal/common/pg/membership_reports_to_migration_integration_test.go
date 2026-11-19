//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds a PRE-migration self-loop (reports_to = id) that no typed helper can produce any more, then asserts the post-Up rows and the constraint.
package pg_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const reportsToNotSelfMigration = 20261120000002

func seedMembership(ctx context.Context, t *testing.T, db *sql.DB, tenantID string, at time.Time) string {
	t.Helper()
	person := uuid.NewString()
	exec(ctx, t, db, `INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, created_at)
		VALUES ($1, $2, 'A', 'B', 'x', $3, $4)`, person, person+"@migr.test", uuid.NewString(), at)
	id := uuid.NewString()
	exec(ctx, t, db, `INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at) VALUES ($1, $2, $3, 'active', $4)`, id, person, tenantID, at)
	return id
}

// TestMigration_MembershipReportsToNotSelf: a stored self-loop is cleared and
// recorded; afterwards the database refuses a self-loop; Down drops the
// constraint and restores exactly the recorded loop.
func TestMigration_MembershipReportsToNotSelf(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, migrationsDir(t), reportsToNotSelfMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}
	now := time.Now().UTC()
	tenantID := seedTenant(ctx, t, db, "reports-to", now)
	loop := seedMembership(ctx, t, db, tenantID, now)
	manager := seedMembership(ctx, t, db, tenantID, now)
	report := seedMembership(ctx, t, db, tenantID, now)
	exec(ctx, t, db, `UPDATE identity.tenant_memberships SET reports_to = id WHERE id = $1`, loop)
	exec(ctx, t, db, `UPDATE identity.tenant_memberships SET reports_to = $2 WHERE id = $1`, report, manager)

	if err := goose.UpToContext(ctx, db, migrationsDir(t), reportsToNotSelfMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	reportsTo := func(id string) sql.NullString {
		var v sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT reports_to::text FROM identity.tenant_memberships WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := reportsTo(loop); v.Valid {
		t.Errorf("self-loop not cleared: reports_to = %q", v.String)
	}
	if v := reportsTo(report); !v.Valid || v.String != manager {
		t.Errorf("a legitimate link was touched: reports_to = %v, want %s", v, manager)
	}
	if n := auditCount(ctx, t, db, tenantID, "identity.membership.migrated"); n != 1 {
		t.Errorf("audit rows = %d, want exactly 1 (the repaired self-loop)", n)
	}
	_, err = db.ExecContext(ctx, `UPDATE identity.tenant_memberships SET reports_to = id WHERE id = $1`, report)
	if err == nil || !strings.Contains(err.Error(), "membership_reports_to_not_self") {
		t.Errorf("self-loop after Up: err = %v, want the CHECK violation", err)
	}

	if err := goose.DownToContext(ctx, db, migrationsDir(t), reportsToNotSelfMigration-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v := reportsTo(report); !v.Valid || v.String != manager {
		t.Errorf("Down changed data: reports_to = %v", v)
	}
	if v := reportsTo(loop); !v.Valid || v.String != loop {
		t.Errorf("Down did not restore the recorded self-loop: reports_to = %v, want %s", v, loop)
	}
	exec(ctx, t, db, `UPDATE identity.tenant_memberships SET reports_to = id WHERE id = $1`, report) // allowed again: the constraint is gone
}
