//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureWith → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:no-synctest — the cascade crosses the real forwarder, router and
// Postgres; a virtual clock cannot order them.

// Hierarchy round 6 policy rules, end to end on the attack world: a no-op
// overrides save changes nothing for the member, an Administrator approves
// user-lifecycle names without holding them, and an owner edits a custom role
// they hold.

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Round6_AnUnchangedOverridesSaveRotatesNothing(t *testing.T) {
	w := newAttackWorldWith(t, true)
	f := w.f
	p := permission.IdentityPermissions
	m, tok := w.spawn(t, "noop", w.roles[role.SystemRoles.Tenant.SalesExecutive])
	ownerTok := w.owner.AccessToken
	alive := func(what string) {
		t.Helper()
		if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tok, nil); r.status != http.StatusOK {
			t.Fatalf("%s: the member's token answers %d %s, want 200", what, r.status, r.body)
		}
	}
	alive("before any save")
	for i := range 3 {
		r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+m.membershipID+"/permission-overrides", ownerTok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: atkRoutePerms(), Revoked: []string{}})
		if r.status != http.StatusNoContent {
			t.Fatalf("identical save %d: %d %s", i, r.status, r.body)
		}
		f.evictStamp(t, m.personID) // as the cascade would: the stored stamp is what the next request is checked against
		alive("after an identical save")
	}
	f.grantOverrides(t, ownerTok, m.membershipID, append(atkRoutePerms(), p.Crm.Leads.Assign), nil)
	f.evictStamp(t, m.personID)
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tok, nil); r.status != http.StatusUnauthorized || errCodeOf(r) != "stale_token" {
		t.Fatalf("after a real change the old token answers %d %s, want 401 stale_token", r.status, r.body)
	}
}

func TestE2E_Round6_AnAdministratorApprovesTheUserLifecycleWithoutHoldingIt(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	approve := func(who, id string) httpResp {
		return w.f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", w.tok(t, who), ports.ApprovePermissionRequestRequest{DecisionReason: "round 6"})
	}
	// adminRO is an Administrator whose tenant.admin was revoked: it holds none of these names.
	// identity.users.create left this list with the owner ruling of 2026-11-21 (admin-tier-only: it is requested by, and approved for, the Administrator tier alone; see TestAttack_AddUsers_*).
	for _, perm := range []string{p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock} {
		id := w.submitFor(t, "exec2", perm)
		if r := approve("adminRO", id); r.status != http.StatusNoContent {
			t.Errorf("an Administrator approves %s without holding it: %d %s, want 204", perm, r.status, r.body)
		}
		if got := w.requestState(t, id); got != "approved" {
			t.Errorf("%s: request is %s, want approved", perm, got)
		}
		w.resetGrants(t, "exec2")
	}
	var rows []attack
	for _, perm := range []string{p.Roles.Assign, p.Users.UpdatePermissions, p.Orders.Payments.Record} {
		id := w.submitFor(t, "exec2", perm)
		rows = append(rows, attack{who: "adminRO", what: "approve " + perm + " (must-hold)", method: http.MethodPost,
			path: "/api/v1/permission-requests/" + id + "/approve", body: ports.ApprovePermissionRequestRequest{DecisionReason: "round 6"},
			status: http.StatusForbidden, code: "grant_exceeds_authority"})
	}
	w.assertRows(t, rows)
}

func TestE2E_Round6_AnOwnerEditsACustomRoleTheyHold(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	custom := w.roles[customRoleName]
	// The world's custom role holds identity.users.create as stale data (owner ruling 2026-11-21:
	// a role below the Administrator tier may not contain it). It cannot be assigned, and an edit
	// that would leave it in the role is refused, until the owner removes it; removing it is allowed.
	if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/grant", w.owner.AccessToken, ports.RolePermissionRequest{Permission: p.Crm.Leads.Read}); r.status != http.StatusForbidden || errCodeOf(r) != "admin_tier_required" {
		t.Fatalf("grant on a role that still holds the stale permission: %d %s, want 403 admin_tier_required", r.status, r.body)
	}
	if r := w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/revoke", w.owner.AccessToken, ports.RolePermissionRequest{Permission: p.Users.Create}); r.status != http.StatusNoContent {
		t.Fatalf("remove the stale permission from the role: %d %s, want 204", r.status, r.body)
	}
	w.f.assignRole(t, w.owner.AccessToken, w.mid(t, "owner2"), custom)
	w.relogin(t, "owner2")
	r := w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/grant", w.tok(t, "owner2"), ports.RolePermissionRequest{Permission: p.Crm.Leads.Read})
	if r.status != http.StatusNoContent {
		t.Fatalf("an owner grants on a custom role they hold: %d %s, want 204", r.status, r.body)
	}
	// another administrator who holds a role may not
	w.f.assignRole(t, w.owner.AccessToken, w.mid(t, "admin"), custom)
	w.relogin(t, "admin")
	r = w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/grant", w.tok(t, "admin"), ports.RolePermissionRequest{Permission: p.Crm.Leads.ReadAll})
	if r.status != http.StatusForbidden {
		t.Fatalf("an administrator grants on a role they hold: %d %s, want 403", r.status, r.body)
	}
}
