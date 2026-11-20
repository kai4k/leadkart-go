//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds the PRE-migration rows (tenants in every status, which no typed helper can force) and asserts the post-Up and post-Down rows.
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

const pendingTenantsMigration = 20261121000002

// Owner ruling 2026-11-21: only an active company's members sign in, and a registered
// company is created active. Every tenant that was registered `pending` (and is fully
// in use) becomes active; everything else is left exactly as it is, and Down restores
// only the rows Up changed.
func TestMigration_IdentityPendingTenantsActive_UpDownUp(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, migrationsDir(t), pendingTenantsMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}

	batch := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	tenantRow := func(slug, status string, activated bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at, activated_at, suspended_at, deletion_scheduled_at, deletion_reason)
			VALUES ($1, $2, $3, $3, $4, $5, CASE WHEN $6::boolean THEN $5::timestamptz ELSE NULL END,
			        CASE WHEN $4 = 'suspended' THEN $5::timestamptz ELSE NULL END,
			        CASE WHEN $4 = 'pending_deletion' THEN $5::timestamptz ELSE NULL END,
			        CASE WHEN $4 = 'pending_deletion' THEN 'closing' ELSE '' END)`,
			id, slug, slug+" Pvt Ltd", status, batch, activated)
		return id
	}
	pendA := tenantRow("pend-a", "pending", false)
	pendB := tenantRow("pend-b", "pending", false)
	active := tenantRow("was-active", "active", true)
	suspended := tenantRow("was-suspended", "suspended", true)
	marked := tenantRow("was-marked", "pending_deletion", true)
	pre := tenantSnapshot(ctx, t, db)

	up := func() {
		t.Helper()
		if err := goose.UpToContext(ctx, db, migrationsDir(t), pendingTenantsMigration); err != nil {
			t.Fatalf("up: %v", err)
		}
	}
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, migrationsDir(t), pendingTenantsMigration-1); err != nil {
			t.Fatalf("down: %v", err)
		}
	}

	up()
	afterUp := tenantSnapshot(ctx, t, db)

	t.Run("every pending tenant is active and activated, nothing else changed", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		for _, id := range []string{pendA, pendB} {
			var status string
			var activated sql.NullTime
			if err := db.QueryRowContext(ctx, `SELECT status, activated_at FROM identity.tenants WHERE id = $1`, id).Scan(&status, &activated); err != nil {
				t.Fatal(err)
			}
			if status != "active" || !activated.Valid {
				t.Errorf("tenant %s: status=%s activated_at valid=%v, want active and stamped", id, status, activated.Valid)
			}
		}
		for id, want := range map[string]string{active: "active", suspended: "suspended", marked: "pending_deletion"} {
			var status string
			var activated time.Time
			if err := db.QueryRowContext(ctx, `SELECT status, activated_at FROM identity.tenants WHERE id = $1`, id).Scan(&status, &activated); err != nil {
				t.Fatal(err)
			}
			if status != want || !activated.Equal(batch) {
				t.Errorf("tenant %s: status=%s activated_at=%v, want %s untouched at %v", id, status, activated, want, batch)
			}
		}
	})

	t.Run("one ledger row per changed tenant, in that tenant, system actor", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry
			WHERE action = 'identity.tenant.migrated' AND payload->>'migration' = '20261121000002_identity_pending_tenants_active'
			  AND user_id IS NULL AND tenant_id::text = payload->>'tenant_id' AND tenant_id IN ($1::uuid, $2::uuid)`, pendA, pendB).Scan(&n); err != nil || n != 2 {
			t.Fatalf("ledger rows for the two pending tenants = %d (%v), want 2", n, err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry WHERE action = 'identity.tenant.migrated'`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("ledger rows in total = %d (%v), want 2 (nothing else was changed)", n, err)
		}
	})

	t.Run("up, down, up", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		down()
		if restored := tenantSnapshot(ctx, t, db); restored != pre {
			t.Errorf("Down did not restore the pre-migration state:\n%s", lineDiff(pre, restored))
		}
		up()
		if again := tenantSnapshot(ctx, t, db); again != afterUp {
			t.Errorf("up, down, up differs from up:\n%s", lineDiff(afterUp, again))
		}
	})

	t.Run("Down touches only its own rows", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		// A tenant registered after Up (created active), one of the migrated ones suspended
		// since, and forged ledger rows for a tenant Up never changed.
		after := tenantRow("registered-after", "active", true)
		exec(ctx, t, db, `UPDATE identity.tenants SET status = 'suspended', suspended_at = now() WHERE id = $1`, pendA)
		exec(ctx, t, db, `INSERT INTO common.audit_log_entry (id, action, user_id, tenant_id, occurred_at_utc, duration_ms, succeeded, payload)
			VALUES ($1, 'identity.tenant.migrated', $2, $3, now(), 0, true, $4::jsonb)`, uuid.NewString(), uuid.NewString(), active,
			`{"migration":"20261121000002_identity_pending_tenants_active","tenant_id":"`+active+`","after":{"activated_at":"`+batch.Format(time.RFC3339)+`"}}`)
		exec(ctx, t, db, `INSERT INTO common.audit_log_entry (id, action, user_id, tenant_id, occurred_at_utc, duration_ms, succeeded, payload)
			VALUES ($1, 'identity.tenant.migrated', NULL, $2, now(), 0, true, $3::jsonb)`, uuid.NewString(), suspended,
			`{"migration":"20261121000002_identity_pending_tenants_active","tenant_id":"`+active+`","after":{"activated_at":"`+batch.Format(time.RFC3339)+`"}}`)
		down()
		want := map[string]string{
			pendA:     "suspended",        // changed since Up: left alone
			pendB:     "pending",          // Up's own row: restored
			active:    "active",           // already active before Up: never touched (forged ledger row ignored)
			suspended: "suspended",        // forged ledger row naming another tenant: ignored
			marked:    "pending_deletion", // never in the ledger
			after:     "active",           // registered after Up: never touched
		}
		for id, status := range want {
			var got string
			if err := db.QueryRowContext(ctx, `SELECT status FROM identity.tenants WHERE id = $1`, id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != status {
				t.Errorf("tenant %s is %s after Down, want %s", id, got, status)
			}
		}
		var activated sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT activated_at FROM identity.tenants WHERE id = $1`, pendB).Scan(&activated); err != nil || activated.Valid {
			t.Errorf("a restored tenant has activated_at %v (%v), want NULL", activated, err)
		}
	})
}

// tenantSnapshot renders the status columns the migration can touch; activated_at is
// reduced to "is it set" (Up stamps the migration time, which differs between two Ups).
func tenantSnapshot(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT slug, status, (activated_at IS NOT NULL)::text, (suspended_at IS NOT NULL)::text FROM identity.tenants ORDER BY slug`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var a, c, d, e string
		if err := rows.Scan(&a, &c, &d, &e); err != nil {
			t.Fatal(err)
		}
		b.WriteString(a + "|" + c + "|" + d + "|" + e + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
