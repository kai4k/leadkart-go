//go:build integration

// arch-test:raw-sql-justified — plants STALE data (an override, a role permission, a pending request) that only existed before the owner ruling of 2026-11-21 and that the routes now refuse to write; no typed helper can produce it.

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite: who may add users. Owner ruling 2026-11-21: "SuperAdmin and
// Platform Team, Company Admin and Company Owner are allowed to add users only."
// Every persona below the Administrator tier is refused on every route that adds
// a member and on every way of obtaining the permission, even though the world
// holds the permission as STALE DATA (an override on every persona below the tier
// and in the custom role), the way an existing tenant does from before the
// ruling; the Owner, the Administrators and the operators succeed.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// atkBelowTheTier are the personas below the Administrator tier.
var atkBelowTheTier = []string{"senior", "sales", "hr", "lead", "custom", "exec", "exec2", "norole"}

// atkAtTheTier are the personas at the tier that add users: the second owner,
// the Administrator and the Office Administrator (level 10, the admin tier).
var atkAtTheTier = []string{"owner2", "admin", "office"}

// plantStaleAddUsers writes identity.users.create where only data from before the
// ruling could have put it: an override on every persona below the tier and in
// the custom role's permission set. It writes the rows directly, not through the
// routes (which now refuse it).
func (w *attackWorld) plantStaleAddUsers(t *testing.T) {
	t.Helper()
	create := permission.IdentityPermissions.Users.Create
	tx, err := w.f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.is_platform', 'true', true)`); err != nil {
		t.Fatalf("platform scope: %v", err)
	}
	for _, key := range atkBelowTheTier {
		if _, err := tx.Exec(t.Context(), `INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at)
			VALUES ($1, $2, 'granted', $3, now()) ON CONFLICT (membership_id, permission_name) DO UPDATE SET kind = 'granted', expires_at = NULL`,
			w.who[key].membershipID, create, w.owner.TenantID); err != nil {
			t.Fatalf("plant override for %s: %v", key, err)
		}
	}
	if _, err := tx.Exec(t.Context(), `UPDATE identity.roles SET permissions = permissions || to_jsonb($1::text) WHERE id = $2 AND tenant_id = $3`,
		create, w.roles[customRoleName], w.owner.TenantID); err != nil {
		t.Fatalf("plant role permission: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// plantStalePendingRequest stores a pending request for identity.users.create by a
// member below the tier, as one submitted before the ruling would be.
func (w *attackWorld) plantStalePendingRequest(t *testing.T, who string) string {
	t.Helper()
	id := ids.NewV7().String()
	tx, err := w.f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.is_platform', 'true', true)`); err != nil {
		t.Fatalf("platform scope: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO identity.permission_requests (id, tenant_id, requester_membership_id, permission_constant, duration_days, reason, state)
		VALUES ($1, $2, $3, $4, 7, 'stored before the owner ruling of 2026-11-21', 'pending')`,
		id, w.owner.TenantID, w.who[who].membershipID, permission.IdentityPermissions.Users.Create); err != nil {
		t.Fatalf("plant request: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	w.watched = append(w.watched, id)
	return id
}

func addUserBody(tag string) ports.CreateUserRequest {
	id := ids.NewV7().String()
	return ports.CreateUserRequest{Email: tag + "-" + id[len(id)-8:] + "@e2e.test", Password: "Added-Pass-2026!" + id[len(id)-6:], FirstName: tag, LastName: "Added"}
}

// TestAttack_AddUsers_NobodyBelowTheAdministratorTierAddsAUser: POST /users for
// every persona below the tier, each holding the permission as stale data, is 403
// and writes nothing; the personas at the tier succeed.
func TestAttack_AddUsers_NobodyBelowTheAdministratorTierAddsAUser(t *testing.T) {
	w := newAttackWorld(t)
	var rows []attack
	for _, who := range atkBelowTheTier {
		rows = append(rows, attack{who: who, what: "POST /users", method: http.MethodPost, path: "/api/v1/users", body: addUserBody(who), status: http.StatusForbidden, code: "forbidden"})
	}
	// the Administrator whose tenant.admin was removed (an existing tenant's) holds no create either
	rows = append(rows, attack{who: "adminRO", what: "POST /users", method: http.MethodPost, path: "/api/v1/users", body: addUserBody("adminro"), status: http.StatusForbidden, code: "forbidden"})
	w.assertGrouped(t, rows)
	for _, who := range atkAtTheTier {
		if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/users", w.tok(t, who), addUserBody(who)); r.status != http.StatusCreated {
			t.Errorf("%s adds a user: %d %s, want 201", who, r.status, r.body)
		}
	}
	if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/users", w.owner.AccessToken, addUserBody("owner")); r.status != http.StatusCreated {
		t.Errorf("the owner adds a user: %d %s, want 201", r.status, r.body)
	}
}

// capabilities: the stale grant does not reach effective_permissions, so the
// front-end hides "Add member" for everyone below the tier and shows it at it.
func TestAttack_AddUsers_CapabilitiesDoNotListItBelowTheTier(t *testing.T) {
	w := newAttackWorld(t)
	create := permission.IdentityPermissions.Users.Create
	has := func(who string) bool {
		r := w.f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", w.tok(t, who), nil)
		var c ports.CapabilitiesDto
		if r.status != http.StatusOK || json.Unmarshal(r.body, &c) != nil {
			t.Fatalf("capabilities of %s: %d %s", who, r.status, r.body)
		}
		return slices.Contains(c.EffectivePermissions, create)
	}
	for _, who := range atkBelowTheTier {
		if has(who) {
			t.Errorf("%s: effective_permissions lists identity.users.create", who)
		}
	}
	for _, who := range append([]string{"owner"}, atkAtTheTier...) {
		if !has(who) {
			t.Errorf("%s: effective_permissions does not list identity.users.create", who)
		}
	}
}

// TestAttack_AddUsers_NoWayToObtainItBelowTheTier: by role edit, by override, by
// request and by approval, nothing changes.
func TestAttack_AddUsers_NoWayToObtainItBelowTheTier(t *testing.T) {
	w := newAttackWorld(t)
	create := permission.IdentityPermissions.Users.Create
	const refused = "admin_tier_required"
	shell, custom := w.roles[shellRoleName], w.roles[customRoleName]
	var rows []attack
	// role edits, by the owner (who holds everything): a role below the tier cannot contain it
	rows = append(rows,
		attack{who: "owner", what: "grant it to an executive-level role", method: http.MethodPost, path: "/api/v1/roles/" + shell + "/permissions/grant", body: ports.RolePermissionRequest{Permission: create}, status: http.StatusForbidden, code: refused},
		attack{who: "owner", what: "replace an executive-level role's set with it", method: http.MethodPut, path: "/api/v1/roles/" + shell + "/permissions", body: ports.ReplaceRolePermissionsRequest{Permissions: []string{create}}, status: http.StatusForbidden, code: refused},
		attack{who: "owner", what: "grant it to the custom level-40 role", method: http.MethodPost, path: "/api/v1/roles/" + custom + "/permissions/grant", body: ports.RolePermissionRequest{Permission: create}, status: http.StatusForbidden, code: refused},
		attack{who: "owner", what: "assign the stale custom role (it holds it) to a member", method: http.MethodPost, path: "/api/v1/users/" + w.mid(t, "norole") + "/roles", body: ports.AssignUserRoleRequest{RoleID: custom}, status: http.StatusForbidden, code: refused},
	)
	// overrides, by the owner and by the Administrator, onto every persona below the tier
	for _, who := range atkBelowTheTier {
		for _, by := range []string{"owner", "admin"} {
			rows = append(rows, attack{who: by, what: "override it onto " + who, method: http.MethodPatch, path: "/api/v1/users/" + w.mid(t, who) + "/permission-overrides",
				body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{create}, Revoked: []string{}}, status: http.StatusForbidden, code: refused})
		}
	}
	// requests, by every persona below the tier that may ask for anything at all
	for _, who := range []string{"senior", "sales", "hr", "lead", "exec", "exec2"} {
		rows = append(rows, attack{who: who, what: "request it", method: http.MethodPost, path: "/api/v1/permission-requests",
			body: ports.CreatePermissionRequestRequest{Permission: create, Reason: elevationReason}, status: http.StatusForbidden, code: refused})
	}
	// approvals of stored requests, by the Administrator and the owner
	for _, who := range []string{"exec", "lead", "sales"} {
		id := w.plantStalePendingRequest(t, who)
		for _, by := range []string{"admin", "owner"} {
			rows = append(rows, attack{who: by, what: "approve it for " + who, method: http.MethodPost, path: "/api/v1/permission-requests/" + id + "/approve",
				body: ports.ApprovePermissionRequestRequest{DecisionReason: "ok"}, status: http.StatusForbidden, code: refused})
		}
	}
	w.assertRows(t, rows)
}

// The platform side keeps adding users: a super user, an operator holding
// platform.users.create or platform.users.manage (through the tenant-scope
// header) and an impersonation session succeed; an operator holding neither
// does not.
func TestAttack_AddUsers_TheOperatorsStillAddUsers(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	hdr := scopeHdr(w.owner.TenantID)
	mint := func(super bool, perms ...string) string {
		// platform.tenants.view is what lets an operator select a foreign tenant at all
		return mintOperatorJWTExact(t, f.Issuer, f.persons, "", append([]string{p.Platform.TenantsView}, perms...), super)
	}
	for _, tc := range []struct {
		name string
		tok  string
		want int
	}{
		{"super user", mint(true), http.StatusCreated},
		{"operator with platform.users.create", mint(false, p.Platform.UsersCreate), http.StatusCreated},
		{"operator with platform.users.manage", mint(false, p.Platform.UsersManage), http.StatusCreated},
		{"operator with platform.users.view only", mint(false, p.Platform.UsersView), http.StatusForbidden},
		{"operator with tenants.manage only", mint(false, p.Platform.TenantsManage), http.StatusForbidden},
	} {
		r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users", tc.tok, addUserBody("op"), hdr)
		if r.status != tc.want {
			t.Errorf("%s adds a user through the scope header: %d %s, want %d", tc.name, r.status, r.body, tc.want)
		}
	}
	_, sess := f.openImpersonation(t, w.owner.TenantID)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", sess.AccessToken, addUserBody("session")); r.status != http.StatusCreated {
		t.Errorf("an impersonation session adds a user: %d %s, want 201", r.status, r.body)
	}
	// the operator path never lifts the rule on the permission itself: the operator
	// cannot hand it to a member below the tier either
	op := mint(false, p.Platform.UsersManage, p.Platform.RolesManage)
	r := f.doWithHeaders(t, http.MethodPatch, "/api/v1/users/"+w.mid(t, "exec")+"/permission-overrides", op,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Users.Create}, Revoked: []string{}}, hdr)
	if r.status != http.StatusForbidden || errCodeOf(r) != "admin_tier_required" {
		t.Errorf("an operator overrides it onto an executive: %d %s, want 403 admin_tier_required", r.status, r.body)
	}
}
