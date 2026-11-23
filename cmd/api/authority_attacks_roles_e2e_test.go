//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goals 1, 3 and 4 through the role routes and the override
// route: reach a rank at or above one's own, obtain or confer a permission one
// does not hold, edit the Company Owner role. Every row is a refusal and every
// refusal leaves the world exactly as it was.

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// roleLevels are the seeded levels (ADR 0069 D1) of the roles the world holds.
func roleLevels() map[string]int {
	r := role.SystemRoles.Tenant
	return map[string]int{r.CompanyOwner: 0, r.Administrator: 10, r.OfficeAdministrator: 10, r.SeniorManager: 15, r.SalesManager: 20,
		r.HrManager: 20, r.TeamLead: 30, customRoleName: 40, r.SalesExecutive: 50, shellRoleName: 60}
}

// TestAttack_Roles_CreateCannotReachOrPassOwnRank is goal 3 through POST /roles:
// every persona creates roles at levels that are invalid (0, negative, 100, huge,
// omitted) or at or above its own rank. Nothing may be created.
func TestAttack_Roles_CreateCannotReachOrPassOwnRank(t *testing.T) {
	w := newAttackWorld(t)
	var rows []attack
	n := 0
	for _, actor := range []string{"admin", "office", "senior", "sales", "hr", "lead", "custom", "exec", "norole"} {
		mk := func(what string, body any, status int, code string) {
			rows = append(rows, attack{who: actor, what: what, method: http.MethodPost, path: "/api/v1/roles", body: body, status: status, code: code})
		}
		named := func(level any) map[string]any {
			n++
			return map[string]any{"name": fmt.Sprintf("Atk Role %d", n), "hierarchy_level": level}
		}
		for _, bad := range []int{0, -1, -50, 100, 1000} {
			mk(fmt.Sprintf("level %d", bad), named(bad), http.StatusUnprocessableEntity, "role_invalid")
		}
		n++
		mk("level omitted", map[string]any{"name": fmt.Sprintf("Atk Role %d", n)}, http.StatusBadRequest, "invalid_body")
		n++
		mk("level null", map[string]any{"name": fmt.Sprintf("Atk Role %d", n), "hierarchy_level": nil}, http.StatusBadRequest, "invalid_body")
		n++
		mk("level as string", rawBody(fmt.Sprintf(`{"name":"Atk Role %d","hierarchy_level":"1"}`, n)), http.StatusBadRequest, "invalid_body")
		rank := w.rank(actor)
		for _, lvl := range []int{1, rank - 1, rank} {
			if lvl < 1 || lvl > 99 {
				continue
			}
			mk(fmt.Sprintf("level %d (rank %d)", lvl, rank), named(lvl), http.StatusForbidden, "insufficient_rank")
		}
	}
	// the Administrator who lost tenant.admin holds no roles.create: the gate answers.
	rows = append(rows, attack{who: "adminRO", what: "create a junior role", method: http.MethodPost, path: "/api/v1/roles",
		body: map[string]any{"name": "Atk RO", "hierarchy_level": 60}, status: http.StatusForbidden, code: "forbidden"})
	w.assertGrouped(t, rows)
}

// TestAttack_Roles_EditsFollowTheLadder is goals 2 and 3 through every role
// command: a persona edits, re-levels, re-parents, empties or deletes each role at
// or above its own level, and the Company Owner role at any level.
func TestAttack_Roles_EditsFollowTheLadder(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	var rows []attack
	for _, actor := range []string{"admin", "office", "senior", "sales", "hr", "lead", "custom", "exec", "norole"} {
		for roleName, lvl := range roleLevels() {
			if lvl > 0 && lvl > w.rank(actor) {
				continue // strictly below the actor: an ordinary edit, not an attack
			}
			id := w.roles[roleName]
			base := "/api/v1/roles/" + id
			status, code := http.StatusForbidden, "insufficient_rank"
			if lvl == 0 {
				status, code = http.StatusConflict, "role_system_default"
			}
			add := func(what, method, path string, body any) {
				rows = append(rows, attack{who: actor, what: what + " " + roleName, method: method, path: path, body: body, status: status, code: code})
			}
			add("rename", http.MethodPatch, base, map[string]any{"name": "Renamed " + roleName})
			add("re-level to 99", http.MethodPatch, base, map[string]any{"hierarchy_level": 99})
			add("re-level to 1", http.MethodPatch, base, map[string]any{"hierarchy_level": 1})
			add("replace permissions with []", http.MethodPut, base+"/permissions", ports.ReplaceRolePermissionsRequest{Permissions: []string{}})
			add("grant a permission", http.MethodPost, base+"/permissions/grant", ports.RolePermissionRequest{Permission: p.Tasks.WorkItems.Read})
			add("revoke tenant.admin", http.MethodPost, base+"/permissions/revoke", ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin})
			add("clear the parent", http.MethodPatch, base+"/parent", map[string]any{"parent_role_id": nil})
			add("delete", http.MethodDelete, base, nil)
		}
	}
	// an Administrator who lost tenant.admin reaches no write route at all.
	for _, c := range []struct{ what, method, path string }{
		{"rename a junior role", http.MethodPatch, "/api/v1/roles/" + w.roles[shellRoleName]},
		{"delete a junior role", http.MethodDelete, "/api/v1/roles/" + w.roles[shellRoleName]},
		{"grant on a junior role", http.MethodPost, "/api/v1/roles/" + w.roles[shellRoleName] + "/permissions/grant"},
	} {
		rows = append(rows, attack{who: "adminRO", what: c.what, method: c.method, path: c.path, body: map[string]any{"name": "x", "permission": p.Tasks.WorkItems.Read},
			status: http.StatusForbidden, code: "forbidden"})
	}
	w.assertGrouped(t, rows)
}

// TestAttack_Roles_GrantCeiling is goal 4 through the role routes and the override
// route. A persona may edit the (holder-less) Shell role and override a role-less
// member, so only the grant ceiling stands in the way: tenant.admin, every
// platform-reserved name and a requestable name the persona does not hold are
// refused; so are unknown, empty and oddly spelt names.
func TestAttack_Roles_GrantCeiling(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	shell := w.roles[shellRoleName]
	target := w.mid(t, "norole")
	notHeld := p.Inventory.Catalog.Manage // no seeded manager role carries it
	var rows []attack
	exceeds := func(actor, perm string) {
		rows = append(rows,
			attack{who: actor, what: "grant " + perm + " to the Shell role", method: http.MethodPost, path: "/api/v1/roles/" + shell + "/permissions/grant",
				body: ports.RolePermissionRequest{Permission: perm}, status: http.StatusForbidden, code: "grant_exceeds_authority"},
			attack{who: actor, what: "replace the Shell role's permissions with " + perm, method: http.MethodPut, path: "/api/v1/roles/" + shell + "/permissions",
				body: ports.ReplaceRolePermissionsRequest{Permissions: []string{perm}}, status: http.StatusForbidden, code: "grant_exceeds_authority"},
			attack{who: actor, what: "override " + perm + " onto a role-less member", method: http.MethodPatch, path: "/api/v1/users/" + target + "/permission-overrides",
				body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{perm}, Revoked: []string{}}, status: http.StatusForbidden, code: "grant_exceeds_authority"})
	}
	reserved := []string{p.Meta.TenantAdmin, p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage, p.Platform.UsersManage, p.Platform.RolesManage,
		p.Platform.ImpersonationManage, p.Platform.TenantsOwnership, p.Tenants.Suspend, p.Tenants.Activate,
		p.PlatformLeadCredits.Topup}
	for _, actor := range []string{"admin", "office", "senior", "sales", "hr", "lead", "custom", "exec"} {
		for _, perm := range reserved {
			exceeds(actor, perm)
		}
	}
	for _, actor := range []string{"senior", "sales", "hr", "lead", "custom", "exec"} {
		exceeds(actor, notHeld)
		exceeds(actor, p.Tenants.UpdateSettings)
	}
	// unknown, empty and oddly spelt names are refused for everyone, administrators included.
	for _, actor := range []string{"admin", "sales", "lead"} {
		// names that normalise to tenant.admin are the real name: refused on the ceiling, like it.
		for _, spelt := range []string{" tenant.admin", "tenant.admin "} {
			exceeds(actor, spelt)
		}
		for _, bad := range []string{"", " ", "no.such.permission", "TENANT.ADMIN", "tenant.admin\u0000", "identity.users.view,tenant.admin", "*", "platform.*"} {
			rows = append(rows,
				attack{who: actor, what: fmt.Sprintf("grant %q", bad), method: http.MethodPost, path: "/api/v1/roles/" + shell + "/permissions/grant",
					body: ports.RolePermissionRequest{Permission: bad}, status: http.StatusUnprocessableEntity},
				attack{who: actor, what: fmt.Sprintf("override %q", bad), method: http.MethodPatch, path: "/api/v1/users/" + target + "/permission-overrides",
					body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{bad}, Revoked: []string{}}, status: http.StatusUnprocessableEntity})
		}
	}
	// a name in both lists, and duplicated, is a validation error before anything is locked.
	for _, actor := range []string{"admin", "sales"} {
		rows = append(rows,
			attack{who: actor, what: "a name in both lists", method: http.MethodPatch, path: "/api/v1/users/" + target + "/permission-overrides",
				body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Tasks.WorkItems.Read}, Revoked: []string{p.Tasks.WorkItems.Read}}, status: http.StatusUnprocessableEntity, code: "validation_failed"})
	}
	w.assertGrouped(t, rows)
}
