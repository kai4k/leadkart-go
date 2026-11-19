//go:build integration

// arch-test:raw-sql-justified — goose data-migration test: seeds the PRE-migration role rows (old seed levels and permission sets) that no current typed helper can produce, then asserts the post-Up and post-Down rows.
package pg_test

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/leadkart/leadkart-go/internal/common/pg"
)

const roleTiersMigration = 20261119000001

// Old (pre-ADR 0069) seed permission sets, one per seeded role the matrix uses.
const (
	oldAdministratorPerms = `["tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]`
	oldSeniorManagerPerms = oldAdministratorPerms
	oldHRManagerPerms     = oldAdministratorPerms
	oldOfficeAdminPerms   = `["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.orders.read","orders.payments.read","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]`
	oldSalesManagerPerms  = `["inventory.catalog.read","inventory.stock.read","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.orders.manage","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]`
	oldPurchaseMgrPerms   = `["inventory.catalog.manage","inventory.stock.manage","tasks.work_items.read","tasks.work_items.read_all","tasks.work_items.manage","tasks.work_items.reassign","notifications.inbox.read","notifications.inbox.manage"]`
	oldSalesExecPerms     = `["tasks.work_items.read","tasks.work_items.manage","orders.quotations.read","orders.quotations.manage","orders.orders.read","orders.payments.read","orders.payments.record","orders.invoices.read","notifications.inbox.read","notifications.inbox.manage"]`
)

type tierRole struct {
	tenant, name                string
	level                       int
	perms                       string
	at                          time.Time
	deleted, system, superAdmin bool
}

// tierWorld is the realistic pre-migration state: one tenant per data shape.
type tierWorld struct {
	alpha, bravo, charlie, delta, echo, foxtrot, platform        string
	floorSupervisor, salesMgrAlpha, purchaseMgrAlpha, adminAlpha string
	edgeRemoved, edgeKept, edgeEqual                             string
	echoTeamLeadDeleted                                          string
}

func TestMigration_IdentityRoleTiers_RealisticPreState(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
		t.Fatalf("up to previous: %v", err)
	}
	w := seedTierWorld(ctx, t, db)
	pre := snapshotRoles(ctx, t, db)

	up := func() {
		t.Helper()
		if err := goose.UpToContext(ctx, db, migrationsDir(t), roleTiersMigration); err != nil {
			t.Fatalf("up: %v", err)
		}
	}
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, migrationsDir(t), roleTiersMigration-1); err != nil {
			t.Fatalf("down: %v", err)
		}
	}

	up()
	afterUp := snapshotRoles(ctx, t, db)

	t.Run("pristine tenant: everything upgrades", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.alpha, "Senior Manager", 15, []string{"identity.roles.approve", "crm.leads.read_all"}, nil)
		assertRoleHas(ctx, t, db, w.alpha, "Sales Manager", 20, []string{"crm.leads.read_all", "identity.roles.approve", "identity.meta.request_permission_elevation"}, nil)
		assertRoleHas(ctx, t, db, w.alpha, "Purchase Manager", 20, []string{"identity.roles.approve"}, nil)
		assertRoleHas(ctx, t, db, w.alpha, "HR Manager", 20, []string{"identity.roles.approve"}, []string{"identity.users.update"})
		assertRoleHas(ctx, t, db, w.alpha, "Sales Executive", 50, []string{"crm.leads.read", "crm.leads.manage"}, nil)
		assertRoleHas(ctx, t, db, w.alpha, "Company Owner", 0, []string{"tenant.admin"}, nil)
		tl := assertRoleHas(ctx, t, db, w.alpha, "Team Lead", 30, []string{"tasks.work_items.reassign", "identity.roles.approve"}, []string{"crm.leads.read_all"})
		var createdAt time.Time
		if err := db.QueryRowContext(ctx, `SELECT created_at FROM identity.roles WHERE id = $1`, tl).Scan(&createdAt); err != nil {
			t.Fatal(err)
		}
		if !createdAt.After(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("Team Lead created_at = %v, want the migration's now(), not the owner batch's stamp", createdAt)
		}
	})

	t.Run("pristine Administrator: level 10 and the read bundle, never tenant.admin", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.alpha, "Administrator", 10, []string{
			"identity.users.view", "identity.roles.view", "identity.tenants.view", "crm.leads.read", "crm.leads.read_all",
			"inventory.catalog.read", "inventory.stock.read", "dispatch.consignment_notes.read",
			"orders.quotations.read", "orders.orders.read", "orders.payments.read", "orders.invoices.read",
			"tasks.work_items.read", "tasks.work_items.read_all", "platform.lead_credits.read", "platform.marketplace.browse",
			"identity.meta.request_permission_elevation",
		}, []string{"tenant.admin", "identity.users.update", "identity.roles.assign", "identity.roles.approve"})
	})

	t.Run("Office Administrator at 50 is unchanged", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.alpha, "Office Administrator", 50, []string{"inventory.catalog.read", "orders.invoices.read"},
			[]string{"identity.meta.request_permission_elevation", "tenant.admin", "identity.users.view"})
	})

	t.Run("customised manager role is untouched and Team Lead is not inserted", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.bravo, "Sales Manager", 40, []string{"orders.orders.manage"}, []string{"identity.roles.approve", "crm.leads.read", "crm.leads.read_all"}) // level customised: untouched whole, permissions too
		assertRoleHas(ctx, t, db, w.bravo, "HR Manager", 50, []string{"tasks.work_items.read"}, []string{"identity.roles.approve"})
		assertRoleHas(ctx, t, db, w.bravo, "Purchase Manager", 50, nil, []string{"identity.roles.approve"}) // tenant-created homonym, newer than the batch
		if n := countRoles(ctx, t, db, w.bravo, "Team Lead"); n != 0 {
			t.Errorf("Team Lead rows in a tenant with a level-40 manager: %d, want 0", n)
		}
		// The other pristine roles of that tenant still upgrade on their own merits.
		assertRoleHas(ctx, t, db, w.bravo, "Senior Manager", 15, []string{"identity.roles.approve"}, nil)
	})

	t.Run("a manager at exactly level 30 blocks Team Lead", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		if n := countRoles(ctx, t, db, w.foxtrot, "Team Lead"); n != 0 {
			t.Errorf("Team Lead rows beside a level-30 manager: %d, want 0", n)
		}
		assertRoleHas(ctx, t, db, w.foxtrot, "Sales Manager", 30, nil, []string{"identity.roles.approve"})
		assertRoleHas(ctx, t, db, w.foxtrot, "Purchase Manager", 50, []string{"crm.leads.read"}, []string{"identity.roles.approve"})
	})

	t.Run("existing custom team lead (different case) blocks the insert", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		if n := countRolesFold(ctx, t, db, w.charlie); n != 1 {
			t.Errorf("roles named team lead in charlie (case-insensitive): %d, want exactly the custom one", n)
		}
		assertRoleHas(ctx, t, db, w.charlie, "TEAM lead ", 50, []string{"crm.leads.read"}, nil)
		assertRoleHas(ctx, t, db, w.charlie, "Sales Manager", 20, []string{"crm.leads.read_all"}, nil)
	})

	t.Run("roles seeded with differing timestamps are not in the batch", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.delta, "Administrator", 10, []string{"crm.leads.read_all"}, nil) // in the batch: upgraded
		assertRoleHas(ctx, t, db, w.delta, "Purchase Manager", 50, []string{"inventory.stock.manage"}, []string{"identity.roles.approve"})
		assertRoleHas(ctx, t, db, w.delta, "Sales Manager", 50, []string{"inventory.stock.read"}, []string{"crm.leads.read_all"})
		if n := countRoles(ctx, t, db, w.delta, "Team Lead"); n != 0 {
			t.Errorf("Team Lead rows beside managers left at 50: %d, want 0", n)
		}
	})

	t.Run("soft-deleted roles are not upgraded and do not block Team Lead", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		var level int
		if err := db.QueryRowContext(ctx, `SELECT hierarchy_level FROM identity.roles WHERE tenant_id = $1 AND name = 'Sales Manager' AND is_deleted`, w.echo).Scan(&level); err != nil || level != 50 {
			t.Errorf("deleted pristine Sales Manager: level=%d err=%v, want untouched 50", level, err)
		}
		assertRoleHas(ctx, t, db, w.echo, "Team Lead", 30, []string{"identity.roles.approve"}, nil)
		var live, gone int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE NOT is_deleted), count(*) FILTER (WHERE is_deleted) FROM identity.roles WHERE tenant_id = $1 AND name = 'Team Lead'`, w.echo).Scan(&live, &gone); err != nil || live != 1 || gone != 1 {
			t.Errorf("echo Team Lead rows live=%d deleted=%d err=%v, want 1 and 1", live, gone, err)
		}
	})

	t.Run("level-0 custom roles move to 50, the SuperAdmin role to 1, other system roles stay", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		assertRoleHas(ctx, t, db, w.echo, "Legacy Level Zero", 50, []string{"crm.leads.read"}, nil)
		assertRoleHas(ctx, t, db, w.alpha, "Legacy Level Zero", 50, nil, nil)
		var level int
		if err := db.QueryRowContext(ctx, `SELECT hierarchy_level FROM identity.roles WHERE tenant_id = $1 AND name = 'Gone Level Zero'`, w.echo).Scan(&level); err != nil || level != 50 {
			t.Errorf("soft-deleted level-0 role: level=%d err=%v, want 50", level, err)
		}
		assertRoleHas(ctx, t, db, w.platform, "SuperAdmin", 1, nil, nil)
		assertRoleHas(ctx, t, db, w.platform, "Some System Role", 0, nil, nil)
		for _, tenant := range []string{w.alpha, w.bravo, w.platform} {
			assertRoleHas(ctx, t, db, tenant, "Company Owner", 0, []string{"tenant.admin"}, nil)
		}
	})

	t.Run("an edge the level change made invalid is soft-removed; a valid edge stays", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		if removedAt, reason := edgeState(ctx, t, db, w.edgeRemoved); removedAt == nil || !strings.Contains(reason, "20261119000001_identity_role_tiers") {
			t.Errorf("invalid edge: removed_at=%v reason=%q, want soft-removed naming the migration", removedAt, reason)
		}
		if removedAt, _ := edgeState(ctx, t, db, w.edgeEqual); removedAt == nil {
			t.Errorf("edge whose parent and child now share level 20 was kept")
		}
		if removedAt, _ := edgeState(ctx, t, db, w.edgeKept); removedAt != nil {
			t.Errorf("valid edge removed at %v", removedAt)
		}
	})

	t.Run("one audit record per changed or created role and per removed edge", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		// alpha: Administrator, Senior, Sales, Purchase, HR managers, Sales Executive upgraded + Legacy Level Zero re-levelled.
		if n := auditCount(ctx, t, db, w.alpha, "identity.role.migrated"); n != 7 {
			t.Errorf("alpha migrated-role audit rows = %d, want 7", n)
		}
		if n := auditCount(ctx, t, db, w.alpha, "identity.role.created"); n != 1 {
			t.Errorf("alpha created-role audit rows = %d, want 1 (Team Lead)", n)
		}
		if n := auditCount(ctx, t, db, w.alpha, "identity.role_edge.removed"); n != 2 {
			t.Errorf("alpha removed-edge audit rows = %d, want 2", n)
		}
		var nullActor, named int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE user_id IS NULL), count(*) FILTER (WHERE payload->>'migration' = '20261119000001_identity_role_tiers' AND length(payload->>'reason') > 10)
			FROM common.audit_log_entry WHERE action LIKE 'identity.role%'`).Scan(&nullActor, &named); err != nil {
			t.Fatal(err)
		}
		var total int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry WHERE action LIKE 'identity.role%'`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if nullActor != total || named != total || total == 0 {
			t.Errorf("audit rows: total=%d system-actor=%d naming migration+reason=%d, want all", total, nullActor, named)
		}
		// No change, no record: bravo's customised roles have none.
		if n := auditCountRole(ctx, t, db, w.bravo, "HR Manager"); n != 0 {
			t.Errorf("untouched bravo HR Manager has %d audit rows, want 0", n)
		}
	})

	t.Run("down restores the pre-migration state exactly, up again is the same as the first up", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		down()
		if diff := diffSnapshots(pre, snapshotRoles(ctx, t, db)); diff != "" {
			t.Fatalf("state after down differs from the pre-migration state:\n%s", diff)
		}
		if n := countRoles(ctx, t, db, w.alpha, "Team Lead"); n != 0 {
			t.Errorf("Team Lead survived down: %d", n)
		}
		if removedAt, _ := edgeState(ctx, t, db, w.edgeRemoved); removedAt != nil {
			t.Errorf("removed edge not restored by down (removed_at=%v)", removedAt)
		}
		up()
		if diff := diffSnapshots(afterUp, snapshotRoles(ctx, t, db)); diff != "" {
			t.Fatalf("second up differs from the first:\n%s", diff)
		}
		if removedAt, _ := edgeState(ctx, t, db, w.edgeRemoved); removedAt == nil {
			t.Errorf("edge not removed again by the second up")
		}
	})

	t.Run("down leaves alone what changed after up", func(t *testing.T) { // arch-test:serial — steps share one migrated database and run in order
		// A tenant admin edits a migrated role, hangs the new Team Lead under an
		// Administrator, and gives echo's Team Lead a holder.
		exec(ctx, t, db, `UPDATE identity.roles SET permissions = permissions || '["inventory.stock.manage"]'::jsonb WHERE tenant_id = $1 AND name = 'Senior Manager'`, w.alpha)
		alphaTL := roleID(ctx, t, db, w.alpha, "Team Lead")
		exec(ctx, t, db, `INSERT INTO identity.role_hierarchy_edges (id, tenant_id, child_role_id, parent_role_id) VALUES ($1, $2, $3, $4)`,
			uuid.NewString(), w.alpha, alphaTL, w.adminAlpha)
		echoTL := roleIDLive(ctx, t, db, w.echo, "Team Lead")
		person, membership := uuid.NewString(), uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.persons (id, email, first_name, last_name, password_hash, security_stamp, created_at)
			VALUES ($1, $2, 'Tee', 'Lead', 'x', $3, now())`, person, person+"@tiers.test", uuid.NewString())
		exec(ctx, t, db, `INSERT INTO identity.tenant_memberships (id, person_id, tenant_id, status, joined_at) VALUES ($1, $2, $3, 'active', now())`, membership, person, w.echo)
		exec(ctx, t, db, `INSERT INTO identity.role_assignments (membership_id, role_id, tenant_id, assigned_at) VALUES ($1, $2, $3, now())`, membership, echoTL, w.echo)

		down()
		assertRoleHas(ctx, t, db, w.alpha, "Senior Manager", 15, []string{"inventory.stock.manage"}, nil) // edited: level and permissions stay
		assertRoleHas(ctx, t, db, w.alpha, "Sales Manager", 50, nil, []string{"crm.leads.read_all"})      // untouched since: reverted
		if n := countRoles(ctx, t, db, w.alpha, "Team Lead"); n != 1 {
			t.Errorf("alpha Team Lead with an active edge: %d rows, want kept", n)
		}
		if n := countRoles(ctx, t, db, w.echo, "Team Lead"); n != 2 { // live + soft-deleted original
			t.Errorf("echo Team Lead rows (one with a holder): %d, want kept (2 incl. the deleted one)", n)
		}
		if n := countRoles(ctx, t, db, w.platform, "Team Lead"); n != 0 {
			t.Errorf("platform Team Lead (no holder, unchanged): %d, want deleted", n)
		}
	})
}

func seedTierWorld(ctx context.Context, t *testing.T, db *sql.DB) tierWorld {
	t.Helper()
	batch := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	var w tierWorld
	w.alpha, w.bravo, w.charlie, w.delta, w.echo = seedTenant(ctx, t, db, "tiers-alpha", batch), seedTenant(ctx, t, db, "tiers-bravo", batch),
		seedTenant(ctx, t, db, "tiers-charlie", batch), seedTenant(ctx, t, db, "tiers-delta", batch), seedTenant(ctx, t, db, "tiers-echo", batch)
	w.platform = seedTenant(ctx, t, db, "tiers-platform", batch)
	w.foxtrot = seedTenant(ctx, t, db, "tiers-foxtrot", batch)
	r := func(x tierRole) string {
		t.Helper()
		if x.at.IsZero() {
			x.at = batch
		}
		id := uuid.NewString()
		exec(ctx, t, db, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at, is_deleted, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::timestamptz, $9::boolean, CASE WHEN $9::boolean THEN $8::timestamptz ELSE NULL END)`,
			id, x.tenant, x.name, x.system, x.superAdmin, x.level, orEmpty(x.perms), x.at, x.deleted)
		return id
	}

	// alpha — a pristine tenant, plus a custom role stored at level 0, an Office Administrator at 50.
	w.adminAlpha = r(tierRole{tenant: w.alpha, name: "Administrator", level: 10, perms: oldAdministratorPerms})
	r(tierRole{tenant: w.alpha, name: "Senior Manager", level: 20, perms: oldSeniorManagerPerms})
	w.salesMgrAlpha = r(tierRole{tenant: w.alpha, name: "Sales Manager", level: 50, perms: oldSalesManagerPerms})
	w.purchaseMgrAlpha = r(tierRole{tenant: w.alpha, name: "Purchase Manager", level: 50, perms: oldPurchaseMgrPerms})
	hrAlpha := r(tierRole{tenant: w.alpha, name: "HR Manager", level: 50, perms: oldHRManagerPerms})
	peer := r(tierRole{tenant: w.alpha, name: "Peer Lead", level: 20, perms: `[]`, at: batch.Add(time.Hour)})
	r(tierRole{tenant: w.alpha, name: "Sales Executive", level: 50, perms: oldSalesExecPerms})
	r(tierRole{tenant: w.alpha, name: "Office Administrator", level: 50, perms: oldOfficeAdminPerms})
	r(tierRole{tenant: w.alpha, name: "Legacy Level Zero", level: 0, perms: `[]`, at: batch.Add(time.Minute)})
	w.floorSupervisor = r(tierRole{tenant: w.alpha, name: "Floor Supervisor", level: 40, perms: `[]`, at: batch.Add(time.Hour)})
	// A valid edge before (40 over 50) that the Sales Manager move to 20 invalidates; a valid one that stays valid.
	w.edgeRemoved = insertEdge(ctx, t, db, w.alpha, w.floorSupervisor, w.salesMgrAlpha)
	w.edgeKept = insertEdge(ctx, t, db, w.alpha, w.adminAlpha, w.purchaseMgrAlpha)
	// 20 over 50 before; the HR Manager move to 20 makes parent and child equal, which is invalid too.
	w.edgeEqual = insertEdge(ctx, t, db, w.alpha, peer, hrAlpha)

	// bravo — customised managers.
	r(tierRole{tenant: w.bravo, name: "Sales Manager", level: 40, perms: oldSalesManagerPerms})
	r(tierRole{tenant: w.bravo, name: "HR Manager", level: 50, perms: `["tasks.work_items.read"]`})
	r(tierRole{tenant: w.bravo, name: "Purchase Manager", level: 50, perms: `[]`, at: batch.Add(time.Hour)})
	r(tierRole{tenant: w.bravo, name: "Senior Manager", level: 20, perms: oldSeniorManagerPerms})

	// charlie — an existing custom team lead in another case; pristine Sales Manager.
	r(tierRole{tenant: w.charlie, name: "TEAM lead ", level: 50, perms: `["crm.leads.read"]`, at: batch.Add(time.Hour)})
	r(tierRole{tenant: w.charlie, name: "Sales Manager", level: 50, perms: oldSalesManagerPerms})

	// delta — roles stamped at different instants (an older seed).
	r(tierRole{tenant: w.delta, name: "Administrator", level: 10, perms: oldAdministratorPerms})
	r(tierRole{tenant: w.delta, name: "Purchase Manager", level: 50, perms: oldPurchaseMgrPerms, at: batch.Add(2 * time.Second)})
	r(tierRole{tenant: w.delta, name: "Sales Manager", level: 50, perms: oldSalesManagerPerms, at: batch.Add(-time.Second)})

	// echo — soft-deleted roles, a deleted Team Lead, level-0 roles.
	r(tierRole{tenant: w.echo, name: "Sales Manager", level: 50, perms: oldSalesManagerPerms, deleted: true})
	w.echoTeamLeadDeleted = r(tierRole{tenant: w.echo, name: "Team Lead", level: 30, perms: `[]`, deleted: true})
	r(tierRole{tenant: w.echo, name: "Legacy Level Zero", level: 0, perms: `["crm.leads.read"]`})
	r(tierRole{tenant: w.echo, name: "Gone Level Zero", level: 0, perms: `[]`, deleted: true})

	// foxtrot — a manager role customised to exactly the Team Lead level.
	r(tierRole{tenant: w.foxtrot, name: "Sales Manager", level: 30, perms: oldSalesManagerPerms})
	// ...and one whose permissions are the old seed set plus an extra grant: customised, so untouched.
	r(tierRole{tenant: w.foxtrot, name: "Purchase Manager", level: 50, perms: strings.Replace(oldPurchaseMgrPerms, `]`, `,"crm.leads.read"]`, 1)})

	// platform — the bootstrap SuperAdmin system role at 0 and another system role at 0.
	r(tierRole{tenant: w.platform, name: "SuperAdmin", level: 0, perms: `[]`, system: true, superAdmin: true})
	r(tierRole{tenant: w.platform, name: "Some System Role", level: 0, perms: `[]`, system: true})
	return w
}

func orEmpty(s string) string {
	if s == "" {
		return `[]`
	}
	return s
}

func exec(ctx context.Context, t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec %.60s: %v", q, err)
	}
}

func insertEdge(ctx context.Context, t *testing.T, db *sql.DB, tenantID, parent, child string) string {
	t.Helper()
	id := uuid.NewString()
	exec(ctx, t, db, `INSERT INTO identity.role_hierarchy_edges (id, tenant_id, child_role_id, parent_role_id) VALUES ($1, $2, $3, $4)`, id, tenantID, child, parent)
	return id
}

func edgeState(ctx context.Context, t *testing.T, db *sql.DB, edge string) (*time.Time, string) {
	t.Helper()
	var at sql.NullTime
	var reason sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT removed_at, removal_reason FROM identity.role_hierarchy_edges WHERE id = $1`, edge).Scan(&at, &reason); err != nil {
		t.Fatalf("edge %s: %v", edge, err)
	}
	if !at.Valid {
		return nil, reason.String
	}
	return &at.Time, reason.String
}

func seedTenant(ctx context.Context, t *testing.T, db *sql.DB, slug string, batch time.Time) string {
	t.Helper()
	id := uuid.NewString()
	exec(ctx, t, db, `INSERT INTO identity.tenants (id, slug, legal_name, display_name, status, created_at, activated_at)
		VALUES ($1, $2, $3, $3, 'active', $4, $4)`, id, slug, slug+" Pvt Ltd", batch)
	exec(ctx, t, db, `INSERT INTO identity.roles (id, tenant_id, name, is_system_default, is_super_admin, hierarchy_level, permissions, created_at)
		VALUES ($1, $2, 'Company Owner', true, false, 0, '["tenant.admin"]'::jsonb, $3)`, uuid.NewString(), id, batch)
	return id
}

// assertRoleHas checks the live role's level, that it holds every perm in has
// and none in lacks. It returns the role id.
func assertRoleHas(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string, wantLevel int, has, lacks []string) string {
	t.Helper()
	var id string
	var level int
	var perms string
	if err := db.QueryRowContext(ctx, `SELECT id, hierarchy_level, permissions::text FROM identity.roles
		WHERE tenant_id = $1 AND name = $2 AND NOT is_deleted`, tenantID, name).Scan(&id, &level, &perms); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if level != wantLevel {
		t.Errorf("%s: level=%d, want %d", name, level, wantLevel)
	}
	for _, p := range has {
		if !strings.Contains(perms, `"`+p+`"`) {
			t.Errorf("%s: missing %s in %s", name, p, perms)
		}
	}
	for _, p := range lacks {
		if strings.Contains(perms, `"`+p+`"`) {
			t.Errorf("%s: unexpectedly holds %s", name, p)
		}
	}
	return id
}

func countRoles(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity.roles WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countRolesFold(ctx context.Context, t *testing.T, db *sql.DB, tenantID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity.roles WHERE tenant_id = $1 AND NOT is_deleted AND lower(btrim(name)) = 'team lead'`, tenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func roleID(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM identity.roles WHERE tenant_id = $1 AND name = $2 ORDER BY is_deleted LIMIT 1`, tenantID, name).Scan(&id); err != nil {
		t.Fatalf("role %s: %v", name, err)
	}
	return id
}

func roleIDLive(ctx context.Context, t *testing.T, db *sql.DB, tenantID, name string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM identity.roles WHERE tenant_id = $1 AND name = $2 AND NOT is_deleted`, tenantID, name).Scan(&id); err != nil {
		t.Fatalf("live role %s: %v", name, err)
	}
	return id
}

func auditCount(ctx context.Context, t *testing.T, db *sql.DB, tenantID, action string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func auditCountRole(ctx context.Context, t *testing.T, db *sql.DB, tenantID, roleName string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM common.audit_log_entry WHERE tenant_id = $1 AND payload->>'role_name' = $2`, tenantID, roleName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// snapshotRoles is every role (live and deleted) and every edge's active flag,
// keyed so two snapshots compare independent of generated ids where the id is
// not meaningful.
func snapshotRoles(ctx context.Context, t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id::text, name, is_deleted, hierarchy_level, permissions::text FROM identity.roles`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tenantID, name, perms string
		var deleted bool
		var level int
		if err := rows.Scan(&tenantID, &name, &deleted, &level, &perms); err != nil {
			t.Fatal(err)
		}
		out[fmt.Sprintf("role %s|%s|deleted=%v", tenantID, name, deleted)] = fmt.Sprintf("level=%d perms=%s", level, sortedPerms(perms))
	}
	_ = rows.Close()
	erows, err := db.QueryContext(ctx, `SELECT id::text, removed_at IS NULL FROM identity.role_hierarchy_edges`)
	if err != nil {
		t.Fatal(err)
	}
	for erows.Next() {
		var id string
		var active bool
		if err := erows.Scan(&id, &active); err != nil {
			t.Fatal(err)
		}
		out["edge "+id] = fmt.Sprintf("active=%v", active)
	}
	_ = erows.Close()
	return out
}

func sortedPerms(jsonArray string) string {
	s := strings.Trim(jsonArray, "[]")
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

func diffSnapshots(want, got map[string]string) string {
	var diffs []string
	for k, w := range want {
		if g, ok := got[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("missing %s (want %s)", k, w))
		} else if g != w {
			diffs = append(diffs, fmt.Sprintf("%s:\n   want %s\n   got  %s", k, w, g))
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("unexpected %s = %s", k, g))
		}
	}
	slices.Sort(diffs)
	return strings.Join(diffs, "\n")
}
