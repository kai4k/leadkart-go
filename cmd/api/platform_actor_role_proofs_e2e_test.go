//go:build integration

// arch-test:no-timeout-needed — newAttackWorld / newE2EFixture bound their own startup (testcontainers wait
// strategy plus the 90s context in startWiredPostgresForHTTP); per-request HTTP uses t.Context().

// Role writes are LeadKart platform authority (ADR 0073 addendum), so the rank ladder, the grant ceiling and
// the holder rules of the role commands bind PLATFORM actors: an impersonation session (Admin-equivalent) and
// the platform operator holding platform.roles.manage (exempt, but never over ownership). These are the
// end-to-end proofs of that, plus the platform tenant's own staff (G2l).

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// An impersonation session acts on every holder: a role an Administrator or an owner holds cannot be
// edited, re-levelled, emptied or deleted by it (403 insufficient_rank, before role_in_use), and nothing
// changes.
func TestAttack_PlatformActors_ImpersonationActsOnEveryHolder(t *testing.T) {
	w := newAttackWorld(t)
	f, own := w.f, w.owner.AccessToken
	p := permission.IdentityPermissions
	var rows []attack
	for _, c := range []struct{ name, holder string }{{"Held By Admin", "admin"}, {"Held By Owner", "owner2"}} {
		id := f.customRole(t, own, c.name, 20)
		if r := f.grantRolePerm(t, own, id, p.Inventory.Catalog.Read); r.status != http.StatusNoContent {
			t.Fatalf("seed the permission of %s: %d %s", c.name, r.status, r.body)
		}
		f.assignRole(t, own, w.mid(t, c.holder), id)
		base := "/api/v1/roles/" + id
		row := func(what, method, path string, body any) {
			rows = append(rows, attack{who: "session", what: what + " (" + c.name + ")", method: method, path: path, body: body, status: http.StatusForbidden, code: "insufficient_rank"})
		}
		row("grant", http.MethodPost, base+"/permissions/grant", ports.RolePermissionRequest{Permission: p.Tasks.WorkItems.Read})
		row("revoke", http.MethodPost, base+"/permissions/revoke", ports.RolePermissionRequest{Permission: p.Inventory.Catalog.Read})
		row("replace", http.MethodPut, base+"/permissions", ports.ReplaceRolePermissionsRequest{Permissions: []string{p.Crm.Leads.Read}})
		row("re-level", http.MethodPatch, base, map[string]any{"hierarchy_level": 25})
		row("deactivate", http.MethodPost, base+"/deactivate", nil)
	}
	session := w.impersonationToken(t)
	for i := range rows {
		rows[i].tok = session
	}
	w.assertRows(t, rows)
}

// The level ladder for an impersonation session (Admin-equivalent, rank 10): it cannot create or re-level a
// role to its own level or above, and a level outside [1, 99] is role_invalid.
func TestAttack_PlatformActors_ImpersonationLevelLadder(t *testing.T) {
	w := newAttackWorld(t)
	session := w.impersonationToken(t)
	shell := "/api/v1/roles/" + w.roles[shellRoleName]
	var rows []attack
	for _, lvl := range []int{10, 5, 1} {
		rows = append(rows,
			attack{tok: session, who: "session", what: "re-level to a level at or above the session", method: http.MethodPatch, path: shell, body: map[string]any{"hierarchy_level": lvl}, status: http.StatusForbidden, code: "insufficient_rank"},
			attack{tok: session, who: "session", what: "create at a level at or above the session", method: http.MethodPost, path: "/api/v1/roles", body: map[string]any{"name": "Ladder", "hierarchy_level": lvl}, status: http.StatusForbidden, code: "insufficient_rank"})
	}
	for _, lvl := range []int{0, 100, -1} {
		rows = append(rows,
			attack{tok: session, who: "session", what: "re-level outside [1, 99]", method: http.MethodPatch, path: shell, body: map[string]any{"hierarchy_level": lvl}, status: http.StatusUnprocessableEntity, code: "role_invalid"},
			attack{tok: session, who: "session", what: "create outside [1, 99]", method: http.MethodPost, path: "/api/v1/roles", body: map[string]any{"name": "Ladder", "hierarchy_level": lvl}, status: http.StatusUnprocessableEntity, code: "role_invalid"})
	}
	w.assertRows(t, rows)
}

// The platform operator (exempt in every tenant, but never over ownership) cannot take tenant.admin off a
// custom role, nor delete or restore a role that carries it: tenant.admin is conferred and removed only with
// platform.tenants.ownership.
func TestE2E_PlatformOperator_WithoutOwnership_CannotTouchTenantAdminOnARole(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "opnoown")
	p := permission.IdentityPermissions
	own := owner.AccessToken
	role := f.customRole(t, own, "Carries Admin", 60)
	base := "/api/v1/roles/" + role
	// The platform super user (the only platform tier above ownership) puts tenant.admin on the role.
	super := f.mintPlatformTokenWith(t, nil, true)
	hdr := scopeHdr(owner.TenantID)
	if r := f.doWithHeaders(t, http.MethodPost, base+"/permissions/grant", super, ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}, hdr); r.status != http.StatusNoContent {
		t.Fatalf("super user grants tenant.admin to the role: %d %s", r.status, r.body)
	}
	unchanged := func(t *testing.T, active bool) {
		t.Helper()
		got := f.roleDto(t, own, role)
		has := false
		for _, n := range got.Permissions {
			has = has || n == p.Meta.TenantAdmin
		}
		if !has || got.IsActive != active {
			t.Fatalf("a refused call changed the role: tenant.admin=%v active=%v, want true/%v", has, got.IsActive, active)
		}
	}
	refused := func(what string, r httpResp) {
		t.Helper()
		// The ownership refusal, not a gate 403: the operator reached the policy and was stopped there.
		f.wantForbidden(t, what, r, http.StatusForbidden, "ownership_authority_required")
	}
	refused("revoke tenant.admin", f.roleWrite(t, http.MethodPost, base+"/permissions/revoke", own, ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}))
	refused("replace the permissions without tenant.admin", f.roleWrite(t, http.MethodPut, base+"/permissions", own, ports.ReplaceRolePermissionsRequest{Permissions: []string{p.Crm.Leads.Read}}))
	refused("deactivate a role carrying tenant.admin", f.roleWrite(t, http.MethodPost, base+"/deactivate", own, nil))
	unchanged(t, true)
	// The super user may delete it; the operator cannot bring it back.
	if r := f.doWithHeaders(t, http.MethodPost, base+"/deactivate", super, nil, hdr); r.status != http.StatusOK {
		t.Fatalf("super user deactivates the role: %d %s", r.status, r.body)
	}
	refused("reactivate a role carrying tenant.admin", f.roleWrite(t, http.MethodPost, base+"/reactivate", own, nil))
	unchanged(t, false)
}

// LeadKart's own staff manage LeadKart's roles: the platform tenant's owner (not a super user, no
// platform.roles.manage) creates, edits and deletes a role inside the platform tenant, as before the
// ruling; a company owner still cannot (G2l S1).
func TestE2E_PlatformTenantOwner_ManagesLeadKartRoles(t *testing.T) {
	f := newE2EFixture(t)
	platform := f.registerPlatformTenant(t)
	company := f.registerAndLogin(t, "acmeown")
	tok := platform.AccessToken

	r, id := f.createRoleVia(t, tok, ports.CreateRoleRequest{Name: "LeadKart Support", HierarchyLevel: 60})
	if r.status != http.StatusCreated {
		t.Fatalf("platform-tenant owner creates a role: %d %s, want 201", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodPatch, "/api/v1/roles/"+id, tok, ports.UpdateRoleRequest{Name: "LeadKart Support L2"}, nil); r.status != http.StatusNoContent {
		t.Fatalf("platform-tenant owner renames it: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles/"+id+"/permissions/grant", tok, ports.RolePermissionRequest{Permission: permission.IdentityPermissions.Crm.Leads.Read}, nil); r.status != http.StatusNoContent {
		t.Fatalf("platform-tenant owner grants on it: %d %s", r.status, r.body)
	}
	// The owner self-holder exemption (rank 0 may edit a role it holds itself): a super user gives the
	// owner the role, and the owner still edits it.
	super := f.mintPlatformTokenWith(t, nil, true)
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+platform.MembershipID+"/roles", super, ports.AssignUserRoleRequest{RoleID: id}, scopeHdr(platform.TenantID)); r.status != http.StatusNoContent {
		t.Fatalf("a super user gives the platform owner the role: %d %s", r.status, r.body)
	}
	tok = f.loginToken(t, "platform-owner@e2e.test", "Correct-Horse-Battery-9!platform")
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles/"+id+"/permissions/grant", tok, ports.RolePermissionRequest{Permission: permission.IdentityPermissions.Crm.Leads.ReadAll}, nil); r.status != http.StatusNoContent {
		t.Fatalf("platform-tenant owner edits a role it holds itself: %d %s, want 204", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodDelete, "/api/v1/users/"+platform.MembershipID+"/roles/"+id, super, nil, scopeHdr(platform.TenantID)); r.status != http.StatusNoContent {
		t.Fatalf("a super user takes the role off the platform owner: %d %s", r.status, r.body)
	}
	tok = f.loginToken(t, "platform-owner@e2e.test", "Correct-Horse-Battery-9!platform") // the edit and the revoke rotated the holder's stamp
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles/"+id+"/deactivate", tok, nil, nil); r.status != http.StatusOK {
		t.Fatalf("platform-tenant owner deletes it: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", company.AccessToken, ports.CreateRoleRequest{Name: "Company Role", HierarchyLevel: 60}, asCompanyHdr); r.status != http.StatusForbidden {
		t.Fatalf("a company owner creates a role: %d %s, want 403", r.status, r.body)
	}
}
