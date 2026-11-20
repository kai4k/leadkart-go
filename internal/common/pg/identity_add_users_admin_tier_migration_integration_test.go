//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds the PRE-migration rows (a permission held below the tier, which no current typed helper can produce) and asserts the post-Up and post-Down rows.
package pg_test

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const addUsersTierMigration = 20261121000001

const createPerm = "identity.users.create"

type addUsersWorld struct {
	alpha, bravo string
	// roles
	adminA, mgrA, customA, lowAdminA, deletedMgrA, strayZeroA, mgrB string
	// members
	mgrMemA, adminMemA, noRoleMemA, execMemA, adminOnlyOverrideA, mixedA, mgrMemB string
	// requests
	reqNoRoleA, reqExecA, reqAdminA, reqApprovedA, reqMgrB string
}

func TestMigration_IdentityAddUsersAdminTierOnly_UpDownUp(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, migrationsDir(t), addUsersTierMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}
	w := seedAddUsersWorld(ctx, t, db)
	pre := addUsersSnapshot(ctx, t, db)

	up := func() {
		t.Helper()
		if err := goose.UpToContext(ctx, db, migrationsDir(t), addUsersTierMigration); err != nil {
			t.Fatalf("up: %v", err)
		}
	}
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, migrationsDir(t), addUsersTierMigration-1); err != nil {
			t.Fatalf("down: %v", err)
		}
	}

	up()
	afterUp := addUsersSnapshot(ctx, t, db)

	t.Run("roles below the tier lose it, everything else is untouched", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		for _, id := range []string{w.mgrA, w.customA, w.strayZeroA, w.mgrB} {
			if rolePerms(ctx, t, db, id).has(createPerm) {
				t.Errorf("role %s still holds %s", id, createPerm)
			}
		}
		if !rolePerms(ctx, t, db, w.mgrA).has("crm.leads.read") {
			t.Error("the manager role lost its other permission")
		}
		for _, id := range []string{w.adminA, w.lowAdminA} { // level 10 holds it through tenant.admin; a level-5 custom role holds it explicitly
			if want := id == w.lowAdminA; rolePerms(ctx, t, db, id).has(createPerm) != want {
				t.Errorf("role %s: holds %s = %v, want %v", id, createPerm, !want, want)
			}
		}
		if !rolePerms(ctx, t, db, w.deletedMgrA).has(createPerm) {
			t.Error("a soft-deleted role was edited")
		}
	})

	t.Run("overrides: deleted below the tier, kept at it, a revoke and other grants kept", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		for _, m := range []string{w.mgrMemA, w.noRoleMemA, w.execMemA, w.mgrMemB} {
			if overrideKind(ctx, t, db, m, createPerm) != "" {
				t.Errorf("member %s still has an override for %s", m, createPerm)
			}
		}
		if overrideKind(ctx, t, db, w.adminMemA, createPerm) != "granted" {
			t.Error("an Administrator lost its override")
		}
		if overrideKind(ctx, t, db, w.mixedA, createPerm) != "revoked" {
			t.Error("a REVOKED override was removed")
		}
		if overrideKind(ctx, t, db, w.mgrMemA, "crm.leads.assign") != "granted" {
			t.Error("another granted override of the member was removed")
		}
	})

	t.Run("requests: pending ones below the tier are cancelled, the rest untouched", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		for _, id := range []string{w.reqNoRoleA, w.reqExecA, w.reqMgrB} {
			if got := requestState(ctx, t, db, id); got != "cancelled" {
				t.Errorf("request %s is %s, want cancelled", id, got)
			}
		}
		if got := requestState(ctx, t, db, w.reqAdminA); got != "pending" {
			t.Errorf("an Administrator's request is %s, want pending", got)
		}
		if got := requestState(ctx, t, db, w.reqApprovedA); got != "approved" {
			t.Errorf("an approved request is %s, want approved", got)
		}
	})

	t.Run("every change is in the ledger, in its own tenant, system actor", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		for action, want := range map[string]int{"identity.role.migrated": 4, "identity.membership_override.migrated": 4, "identity.permission_request.migrated": 3} {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry
				WHERE action = $1 AND payload->>'migration' = '20261121000001_identity_add_users_admin_tier_only' AND user_id IS NULL AND tenant_id IS NOT NULL`, action).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != want {
				t.Errorf("%s ledger rows = %d, want %d", action, n, want)
			}
		}
	})

	t.Run("a second Up changes nothing", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		down()
		restored := addUsersSnapshot(ctx, t, db)
		if restored != pre {
			t.Errorf("Down did not restore the pre-migration state:\n%s", lineDiff(pre, restored))
		}
		up()
		if again := addUsersSnapshot(ctx, t, db); again != afterUp {
			t.Errorf("up, down, up differs from up:\n%s", lineDiff(afterUp, again))
		}
	})

	t.Run("Down restores only what Up removed", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		// a role edited since Up is left alone
		exec(ctx, t, db, `UPDATE identity.roles SET permissions = permissions || '["crm.leads.manage"]'::jsonb WHERE id = $1`, w.customA)
		// a member given a REVOKED row for the permission since Up keeps it (no resurrected grant)
		exec(ctx, t, db, `INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at)
			VALUES ($1, $2, 'revoked', $3, now())`, w.noRoleMemA, createPerm, w.alpha)
		// a new pending request made since Up (the code refuses them, but the data may hold one) blocks re-opening the cancelled one
		exec(ctx, t, db, `INSERT INTO identity.permission_requests (id, tenant_id, requester_membership_id, permission_constant, duration_days, reason, state)
			VALUES ($1, $2, $3, $4, 7, 'a newer request for the same permission', 'pending')`, uuid.NewString(), w.alpha, w.execMemA, createPerm)
		// forged ledger rows: a non-system actor, and a tenant that does not own the member
		exec(ctx, t, db, `INSERT INTO common.audit_log_entry (id, action, user_id, tenant_id, occurred_at_utc, duration_ms, succeeded, payload)
			VALUES ($1, 'identity.membership_override.migrated', $2, $3, now(), 0, true, $4::jsonb)`, uuid.NewString(), uuid.NewString(), w.alpha,
			`{"migration":"20261121000001_identity_add_users_admin_tier_only","membership_id":"`+w.adminOnlyOverrideA+`","permission":"identity.users.create","before":{"kind":"granted"}}`)
		exec(ctx, t, db, `INSERT INTO common.audit_log_entry (id, action, user_id, tenant_id, occurred_at_utc, duration_ms, succeeded, payload)
			VALUES ($1, 'identity.membership_override.migrated', NULL, $2, now(), 0, true, $3::jsonb)`, uuid.NewString(), w.bravo,
			`{"migration":"20261121000001_identity_add_users_admin_tier_only","membership_id":"`+w.adminOnlyOverrideA+`","permission":"identity.users.create","before":{"kind":"granted"}}`)
		down()
		if !rolePerms(ctx, t, db, w.customA).has("crm.leads.manage") || rolePerms(ctx, t, db, w.customA).has(createPerm) {
			t.Errorf("a role edited since Up was rewritten: %v", rolePerms(ctx, t, db, w.customA))
		}
		if !rolePerms(ctx, t, db, w.mgrA).has(createPerm) {
			t.Error("an untouched role was not restored")
		}
		if got := overrideKind(ctx, t, db, w.noRoleMemA, createPerm); got != "revoked" {
			t.Errorf("the member with a newer revoke has %q, want revoked", got)
		}
		if got := overrideKind(ctx, t, db, w.mgrMemA, createPerm); got != "granted" {
			t.Errorf("an override Up deleted is %q after Down, want granted", got)
		}
		if got := overrideKind(ctx, t, db, w.adminOnlyOverrideA, createPerm); got != "" {
			t.Errorf("a forged ledger row restored an override: %q", got)
		}
		if got := requestState(ctx, t, db, w.reqExecA); got != "cancelled" {
			t.Errorf("a request with a newer pending twin is %s, want it left cancelled", got)
		}
		if got := requestState(ctx, t, db, w.reqNoRoleA); got != "pending" {
			t.Errorf("a cancelled request is %s after Down, want pending", got)
		}
	})
}

// ----- the world -------------------------------------------------------------------------------

func seedAddUsersWorld(ctx context.Context, t *testing.T, db *sql.DB) addUsersWorld {
	t.Helper()
	batch := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var w addUsersWorld
	w.alpha, w.bravo = seedTenant(ctx, t, db, "adduser-alpha", batch), seedTenant(ctx, t, db, "adduser-bravo", batch)
	role := func(tenantID, name string, level int, perms string, deleted bool) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_deleted, deleted_at)
			VALUES ($1, $2, $3, false, false, $4, $5::jsonb, $6::timestamptz, $7::boolean, CASE WHEN $7::boolean THEN $6::timestamptz ELSE NULL END)`,
			id, tenantID, name, level, perms, batch, deleted)
		return id
	}
	w.adminA = role(w.alpha, "Administrator", 10, `["tenant.admin","tasks.work_items.read"]`, false)
	w.lowAdminA = role(w.alpha, "Level Five Creator", 5, `["identity.users.create"]`, false)
	w.mgrA = role(w.alpha, "Sales Manager", 20, `["crm.leads.read","identity.users.create"]`, false)
	w.customA = role(w.alpha, "Custom 40", 40, `["identity.users.create","tasks.work_items.read"]`, false)
	w.deletedMgrA = role(w.alpha, "Gone Manager", 20, `["identity.users.create"]`, true)
	w.strayZeroA = role(w.alpha, "Stray Level Zero", 0, `["identity.users.create"]`, false)
	w.mgrB = role(w.bravo, "Sales Manager", 20, `["identity.users.create"]`, false)
	ownerA := ownerRoleOf(ctx, t, db, w.alpha)
	_ = ownerA

	member := func(tenantID, tag string, roles ...string) string {
		person, mem := uuid.NewString(), uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, is_active, created_at)
			VALUES ($1, $2, 'T', 'M', 'hash', $3, true, now())`, person, tag+"@adduser.test", uuid.NewString())
		exec(ctx, t, db, `INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at) VALUES ($1, $2, $3, 'active', now())`, mem, person, tenantID)
		for _, r := range roles {
			exec(ctx, t, db, `INSERT INTO identity.role_assignments (membership_id, role_id, tenant_id, assigned_at) VALUES ($1, $2, $3, now())`, mem, r, tenantID)
		}
		return mem
	}
	override := func(tenantID, mem, perm, kind string) {
		exec(ctx, t, db, `INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at) VALUES ($1, $2, $3, $4, now())`, mem, perm, kind, tenantID)
	}
	request := func(tenantID, mem, state string) string {
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.permission_requests (id, tenant_id, requester_membership_id, permission_constant, duration_days, reason, state)
			VALUES ($1, $2, $3, $4, 7, 'stored before the owner ruling of 2026-11-21', $5)`, id, tenantID, mem, createPerm, state)
		return id
	}

	w.mgrMemA = member(w.alpha, "mgr-a", w.mgrA)
	override(w.alpha, w.mgrMemA, createPerm, "granted")
	override(w.alpha, w.mgrMemA, "crm.leads.assign", "granted")
	w.adminMemA = member(w.alpha, "admin-a", w.adminA)
	override(w.alpha, w.adminMemA, createPerm, "granted")
	w.noRoleMemA = member(w.alpha, "norole-a")
	override(w.alpha, w.noRoleMemA, createPerm, "granted")
	w.reqNoRoleA = request(w.alpha, w.noRoleMemA, "pending")
	w.execMemA = member(w.alpha, "exec-a", w.customA)
	override(w.alpha, w.execMemA, createPerm, "granted")
	w.reqExecA = request(w.alpha, w.execMemA, "pending")
	w.reqAdminA = request(w.alpha, w.adminMemA, "pending")
	w.reqApprovedA = request(w.alpha, w.mgrMemA, "approved")
	w.mixedA = member(w.alpha, "mixed-a", w.mgrA)
	override(w.alpha, w.mixedA, createPerm, "revoked")
	w.adminOnlyOverrideA = member(w.alpha, "lowadmin-a", w.lowAdminA) // rank 5: keeps whatever it has
	w.mgrMemB = member(w.bravo, "mgr-b", w.mgrB)
	override(w.bravo, w.mgrMemB, createPerm, "granted")
	w.reqMgrB = request(w.bravo, w.mgrMemB, "pending")
	return w
}

func ownerRoleOf(ctx context.Context, t *testing.T, db *sql.DB, tenantID string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM identity.roles WHERE tenant_id = $1 AND name = 'Company Owner'`, tenantID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type permSet []string

func (p permSet) has(name string) bool { return slices.Contains(p, name) }

func rolePerms(ctx context.Context, t *testing.T, db *sql.DB, id string) permSet {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT permissions::text FROM identity.roles WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("role %s: %v", id, err)
	}
	raw = strings.Trim(raw, "[]")
	var out permSet
	for _, p := range strings.Split(raw, ",") {
		if p = strings.Trim(strings.TrimSpace(p), `"`); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func overrideKind(ctx context.Context, t *testing.T, db *sql.DB, membershipID, perm string) string {
	t.Helper()
	var kind sql.NullString
	err := db.QueryRowContext(ctx, `SELECT kind FROM identity.membership_permission_overrides WHERE membership_id = $1 AND permission_name = $2`, membershipID, perm).Scan(&kind)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return kind.String
}

func requestState(ctx context.Context, t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRowContext(ctx, `SELECT state FROM identity.permission_requests WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("request %s: %v", id, err)
	}
	return s
}

// addUsersSnapshot renders everything the migration can touch, ordered.
func addUsersSnapshot(ctx context.Context, t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	dump := func(q string) {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("snapshot %.40s: %v", q, err)
		}
		defer rows.Close()
		for rows.Next() {
			var a, c, d, e sql.NullString
			if err := rows.Scan(&a, &c, &d, &e); err != nil {
				t.Fatal(err)
			}
			b.WriteString(a.String + "|" + c.String + "|" + d.String + "|" + e.String + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	dump(`SELECT id::text, name, hierarchy_level::text, permissions::text FROM identity.roles ORDER BY id`)
	dump(`SELECT membership_id::text, permission_name, kind, coalesce(expires_at::text, '') FROM identity.membership_permission_overrides ORDER BY membership_id, permission_name`)
	dump(`SELECT id::text, state, coalesce(decision_reason, ''), (decided_at IS NOT NULL)::text FROM identity.permission_requests ORDER BY id`)
	return b.String()
}

func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for _, l := range al {
		if !slices.Contains(bl, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range bl {
		if !slices.Contains(al, l) {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}
