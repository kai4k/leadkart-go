//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Attack suite, goals 2 and 6: act across tenants, or reach platform authority
// from inside one. Platform operators with exactly one platform permission each,
// a zero-permission platform member, an exempt operator without
// platform.tenants.ownership, an impersonation session, and ordinary tenant
// members who send a scope header.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// anyRefusal is the set of codes any gate may answer an unauthorised caller with.
const anyRefusal = "forbidden|tenant_scope_forbidden|tenant_scope_read_only|tenant_scope_not_supported|impersonation_not_allowed|insufficient_rank|ownership_authority_required|grant_exceeds_authority"

// TestAttack_Operators_OneNarrowPermissionReachesNothingInATenant is goal 6: an
// operator holding exactly ONE platform permission (every kind), and a
// zero-permission platform member, point a scope header at the tenant and try
// every identity write, a permission-request route and a business-module write.
// None may succeed, with or without the header, and nothing changes.
func TestAttack_Operators_OneNarrowPermissionReachesNothingInATenant(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	hdr := scopeHdr(w.owner.TenantID)
	execRole := w.roles[role.SystemRoles.Tenant.SalesExecutive]
	shell := w.roles[shellRoleName]
	exec, exec2, norole := w.mid(t, "exec"), w.mid(t, "exec2"), w.mid(t, "norole")
	routes := []struct {
		what, method, path string
		body               any
	}{
		{"create a member", http.MethodPost, "/api/v1/users", ports.CreateUserRequest{Email: "op-attack@e2e.test", Password: "Attack-Pass-2026!x", FirstName: "Op", LastName: "Attack"}},
		{"assign a role", http.MethodPost, "/api/v1/users/" + norole + "/roles", ports.AssignUserRoleRequest{RoleID: execRole}},
		{"deactivate a member", http.MethodPost, "/api/v1/users/" + exec2 + "/deactivate", ports.DeactivateUserRequest{Reason: "operator attack"}},
		{"override tenant.admin onto a member", http.MethodPatch, "/api/v1/users/" + exec + "/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Meta.TenantAdmin}, Revoked: []string{}}},
		{"clear a manager", http.MethodDelete, "/api/v1/users/" + exec + "/manager", nil},
		{"edit a profile", http.MethodPatch, "/api/v1/users/" + exec + "/profile", ports.UpdateUserProfileRequest{Designation: "pwned"}},
		{"anonymise a member", http.MethodPost, "/api/v1/users/" + exec + "/anonymise", nil},
		{"create a role", http.MethodPost, "/api/v1/roles", ports.CreateRoleRequest{Name: "Operator Attack", HierarchyLevel: 60}},
		{"grant tenant.admin to a role", http.MethodPost, "/api/v1/roles/" + shell + "/permissions/grant", ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}},
		{"delete a role", http.MethodDelete, "/api/v1/roles/" + shell, nil},
		{"submit a permission request", http.MethodPost, "/api/v1/permission-requests", ports.CreatePermissionRequestRequest{Permission: p.Users.View, Reason: elevationReason}},
		{"suspend the tenant", http.MethodPost, "/api/v1/tenants/" + w.owner.TenantID + "/suspend", map[string]any{"reason": "operator attack"}},
	}
	narrow := []string{p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage, p.Platform.UsersView, p.Platform.UsersCreate,
		p.Platform.UsersManage, p.Platform.RolesView, p.Platform.RolesManage, p.Platform.ImpersonationManage, p.Platform.TenantsOwnership}
	tokens := map[string]string{"zero-permission": mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)}
	for _, perm := range narrow {
		tokens["only "+perm] = mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{perm}, false)
	}
	var rows []attack
	for name, tok := range tokens {
		for _, rt := range routes {
			// the one write a single permission may legitimately make on its own is exercised by the platform-gate suite
			for _, h := range []map[string]string{hdr, nil} {
				if h == nil && rt.method == http.MethodPost && rt.path == "/api/v1/users" {
					continue // a synthetic operator token's tenant does not exist: create-user would fail on the foreign key, an artifact of the fixture
				}
				rows = append(rows, attack{who: name, tok: tok, what: rt.what, method: rt.method, path: rt.path, body: rt.body, hdr: h,
					status: http.StatusForbidden, code: anyRefusal})
			}
		}
	}
	// A routes-by-header lookup that is NOT a 403 would be a finding only if it changed state;
	// a header-less call lands in the operator's own (synthetic) tenant: 403 or 404, never success.
	for i := range rows {
		if rows[i].hdr == nil {
			rows[i].status = 0
		}
	}
	w.assertOperatorRows(t, rows)
}

// assertOperatorRows is [attackWorld.assertGrouped] for rows whose status 0 means
// "any 4xx": the header-less requests land in the operator's own tenant, where the
// member ids do not exist (404) or the gate refuses (403).
func (w *attackWorld) assertOperatorRows(t *testing.T, rows []attack) {
	t.Helper()
	var strict, loose []attack
	for _, a := range rows {
		if a.status == 0 {
			loose = append(loose, a)
		} else {
			strict = append(strict, a)
		}
	}
	w.assertGrouped(t, strict)
	// arch-test:serial — the loose rows share the world snapshot with the strict ones above.
	t.Run("header-less requests", func(t *testing.T) {
		before := w.snapshot(t)
		for _, a := range loose {
			r := w.f.doWithHeaders(t, a.method, a.path, a.tok, a.body, nil)
			if r.status != http.StatusForbidden && r.status != http.StatusNotFound && r.status != http.StatusBadRequest && r.status != http.StatusUnprocessableEntity {
				t.Errorf("[%s %s] %s %s without a scope header: %d %s, want a refusal", a.who, a.what, a.method, a.path, r.status, r.body)
			}
		}
		if after := w.snapshot(t); after != before {
			t.Errorf("header-less operator requests changed the world: %s", snapshotDiff(before, after))
		}
	})
}

// TestAttack_Operators_ExemptWithoutOwnershipCannotChangeWhoOwns is goals 1 and 2
// for the strongest operator short of super user: platform.users.manage +
// platform.roles.manage + platform.tenants.view, no platform.tenants.ownership.
// Every path that changes who owns the tenant is refused with
// ownership_authority_required, including reactivating an owner.
func TestAttack_Operators_ExemptWithoutOwnershipCannotChangeWhoOwns(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	hdr := scopeHdr(w.owner.TenantID)
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage, p.Platform.RolesView, p.Platform.RolesManage}, false)
	ownerRole := w.roles[role.SystemRoles.Tenant.CompanyOwner]
	adminRole := w.roles[role.SystemRoles.Tenant.Administrator]
	shell := w.roles[shellRoleName]
	owner2, norole, exec := w.mid(t, "owner2"), w.mid(t, "norole"), w.mid(t, "exec")
	deny := func(what, method, path string, body any) attack {
		return attack{who: "operator", tok: op, what: what, method: method, path: path, body: body, hdr: hdr, status: http.StatusForbidden, code: "ownership_authority_required"}
	}
	rows := []attack{
		deny("assign Company Owner", http.MethodPost, "/api/v1/users/"+norole+"/roles", ports.AssignUserRoleRequest{RoleID: ownerRole}),
		deny("revoke Company Owner from an owner", http.MethodDelete, "/api/v1/users/"+owner2+"/roles/"+ownerRole, nil),
		deny("assign a role carrying tenant.admin (Administrator)", http.MethodPost, "/api/v1/users/"+norole+"/roles", ports.AssignUserRoleRequest{RoleID: adminRole}),
		deny("revoke Administrator from an admin", http.MethodDelete, "/api/v1/users/"+w.mid(t, "admin")+"/roles/"+adminRole, nil),
		deny("deactivate an owner", http.MethodPost, "/api/v1/users/"+owner2+"/deactivate", ports.DeactivateUserRequest{Reason: "operator attack"}),
		deny("reactivate an owner", http.MethodPost, "/api/v1/users/"+w.goneOwner.membershipID+"/reactivate", nil),
		deny("override tenant.admin onto a member", http.MethodPatch, "/api/v1/users/"+exec+"/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Meta.TenantAdmin}, Revoked: []string{}}),
		deny("revoke tenant.admin from an owner by override", http.MethodPatch, "/api/v1/users/"+owner2+"/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{p.Meta.TenantAdmin}}),
		deny("grant tenant.admin to a role", http.MethodPost, "/api/v1/roles/"+shell+"/permissions/grant", ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}),
		deny("replace a role's permissions with tenant.admin", http.MethodPut, "/api/v1/roles/"+shell+"/permissions", ports.ReplaceRolePermissionsRequest{Permissions: []string{p.Meta.TenantAdmin}}),
		// strip tenant.admin from a role other members hold: the ownership gate again
		deny("revoke tenant.admin from the Administrator role", http.MethodPost, "/api/v1/roles/"+adminRole+"/permissions/revoke", ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}),
		// platform-reserved names are beyond even an exempt operator's ceiling
		{who: "operator", tok: op, what: "grant a platform-reserved name to a role", method: http.MethodPost, path: "/api/v1/roles/" + shell + "/permissions/grant",
			body: ports.RolePermissionRequest{Permission: p.Platform.TenantsOwnership}, hdr: hdr, status: http.StatusForbidden, code: "grant_exceeds_authority|ownership_authority_required"},
		{who: "operator", tok: op, what: "override a platform-reserved name onto a member", method: http.MethodPatch, path: "/api/v1/users/" + exec + "/permission-overrides",
			body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Platform.UsersManage}, Revoked: []string{}}, hdr: hdr, status: http.StatusForbidden, code: "grant_exceeds_authority"},
		// a super-admin-flagged role stays super-user-only, however a role is created
		{who: "operator", tok: op, what: "create a role above any rank", method: http.MethodPost, path: "/api/v1/roles",
			body: map[string]any{"name": "Operator Level Zero", "hierarchy_level": 0}, hdr: hdr, status: http.StatusUnprocessableEntity, code: "role_invalid"},
	}
	w.assertRows(t, rows)
}

// TestAttack_Operators_ImpersonationSessionIsAnAdministratorNotAnOwner is goals 1,
// 2 and 6 for an impersonation session: Admin-equivalent whatever rank it claims,
// never an owner, never an approver or requester, never a platform principal.
func TestAttack_Operators_ImpersonationSessionIsAnAdministratorNotAnOwner(t *testing.T) {
	w := newAttackWorld(t)
	f := w.f
	p := permission.IdentityPermissions
	opTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
	sess := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", opTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: w.owner.TenantID, Reason: "attack suite session", DurationMinutes: 30})
	if sess.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", sess.status, sess.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(sess.body, &out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	tok := out.AccessToken
	reqID := w.submitFor(t, "exec2", p.Inventory.Catalog.Manage)
	ownerRole, adminRole := w.roles[role.SystemRoles.Tenant.CompanyOwner], w.roles[role.SystemRoles.Tenant.Administrator]
	owner2, norole, exec := w.mid(t, "owner2"), w.mid(t, "norole"), w.mid(t, "exec")
	imp := func(what, method, path string, body any, status int, code string) attack {
		return attack{who: "session", tok: tok, what: what, method: method, path: path, body: body, status: status, code: code}
	}
	w.assertRows(t, []attack{
		imp("assign Company Owner", http.MethodPost, "/api/v1/users/"+norole+"/roles", ports.AssignUserRoleRequest{RoleID: ownerRole}, http.StatusForbidden, "insufficient_rank"),
		imp("assign Administrator", http.MethodPost, "/api/v1/users/"+norole+"/roles", ports.AssignUserRoleRequest{RoleID: adminRole}, http.StatusForbidden, "insufficient_rank"),
		imp("deactivate an owner", http.MethodPost, "/api/v1/users/"+owner2+"/deactivate", ports.DeactivateUserRequest{Reason: "session attack"}, http.StatusForbidden, "insufficient_rank"),
		imp("deactivate an administrator", http.MethodPost, "/api/v1/users/"+w.mid(t, "admin")+"/deactivate", ports.DeactivateUserRequest{Reason: "session attack"}, http.StatusForbidden, "insufficient_rank"),
		imp("revoke Company Owner from an owner", http.MethodDelete, "/api/v1/users/"+owner2+"/roles/"+ownerRole, nil, http.StatusForbidden, "insufficient_rank"),
		imp("override tenant.admin onto a member", http.MethodPatch, "/api/v1/users/"+exec+"/permission-overrides",
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Meta.TenantAdmin}, Revoked: []string{}}, http.StatusForbidden, "grant_exceeds_authority|insufficient_rank"),
		imp("create a role at the Administrator level", http.MethodPost, "/api/v1/roles", ports.CreateRoleRequest{Name: "Session Admin", HierarchyLevel: 10}, http.StatusForbidden, "insufficient_rank"),
		imp("edit the Company Owner role", http.MethodPatch, "/api/v1/roles/"+ownerRole, map[string]any{"name": "Renamed"}, http.StatusConflict, "role_system_default"),
		imp("approve a request", http.MethodPost, "/api/v1/permission-requests/"+reqID+"/approve", ports.ApprovePermissionRequestRequest{DecisionReason: "session"}, http.StatusForbidden, "impersonation_not_allowed"),
		imp("deny a request", http.MethodPost, "/api/v1/permission-requests/"+reqID+"/deny", ports.DenyPermissionRequestRequest{DecisionReason: "session"}, http.StatusForbidden, "impersonation_not_allowed"),
		imp("submit a request", http.MethodPost, "/api/v1/permission-requests", ports.CreatePermissionRequestRequest{Permission: p.Users.View, Reason: elevationReason}, http.StatusForbidden, "impersonation_not_allowed"),
		imp("use a platform route", http.MethodGet, "/api/v1/platform/tenants", nil, http.StatusForbidden, ""),
		imp("open another session", http.MethodPost, "/api/v1/platform/impersonation/sessions",
			ports.CreateImpersonationSessionRequest{TargetTenantID: w.victim.TenantID, Reason: "chain attack", DurationMinutes: 30}, http.StatusForbidden, ""),
		imp("scope into the victim tenant by header", http.MethodPost, "/api/v1/users/"+w.vExec.membershipID+"/deactivate",
			ports.DeactivateUserRequest{Reason: "session attack"}, http.StatusNotFound, ""),
	})
}

// TestAttack_Operators_TenantMembersCannotScopeOrReachAcrossTenants is goal 6 for
// ordinary tenant members: a header naming the victim tenant is ignored, a path
// naming it is refused, and neither reads nor changes the victim.
func TestAttack_Operators_TenantMembersCannotScopeOrReachAcrossTenants(t *testing.T) {
	w := newAttackWorld(t)
	p := permission.IdentityPermissions
	vh := scopeHdr(w.victim.TenantID)
	vRole := w.f.roleIDByName(t, w.victim.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	var rows []attack
	for _, actor := range []string{"owner", "owner2", "admin", "office", "senior", "sales", "hr", "lead", "exec"} {
		rows = append(rows,
			attack{who: actor, what: "deactivate a victim member under a victim header", method: http.MethodPost, path: "/api/v1/users/" + w.vExec.membershipID + "/deactivate",
				body: ports.DeactivateUserRequest{Reason: "cross tenant"}, hdr: vh, status: http.StatusNotFound},
			attack{who: actor, what: "assign a victim role to a victim member", method: http.MethodPost, path: "/api/v1/users/" + w.vExec.membershipID + "/roles",
				body: ports.AssignUserRoleRequest{RoleID: vRole}, hdr: vh, status: http.StatusNotFound},
			attack{who: actor, what: "override a victim member", method: http.MethodPatch, path: "/api/v1/users/" + w.vExec.membershipID + "/permission-overrides",
				body: ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Users.View}, Revoked: []string{}}, hdr: vh, status: http.StatusNotFound},
			attack{who: actor, what: "grant to a victim role", method: http.MethodPost, path: "/api/v1/roles/" + vRole + "/permissions/grant",
				body: ports.RolePermissionRequest{Permission: p.Users.View}, hdr: vh, status: http.StatusNotFound},
			attack{who: actor, what: "edit the victim tenant by path", method: http.MethodPatch, path: "/api/v1/tenants/" + w.victim.TenantID + "/profile",
				body: ports.UpdateTenantProfileRequest{LegalName: "Pwned Ltd", DisplayName: "Pwned"}, status: http.StatusForbidden},
			attack{who: actor, what: "edit the victim tenant by path with a matching header", method: http.MethodPatch, path: "/api/v1/tenants/" + w.victim.TenantID + "/profile",
				body: ports.UpdateTenantProfileRequest{LegalName: "Pwned Ltd", DisplayName: "Pwned"}, hdr: vh, status: http.StatusForbidden},
			attack{who: actor, what: "edit the victim tenant by path with their own header", method: http.MethodPatch, path: "/api/v1/tenants/" + w.victim.TenantID + "/profile",
				body: ports.UpdateTenantProfileRequest{LegalName: "Pwned Ltd", DisplayName: "Pwned"}, hdr: scopeHdr(w.owner.TenantID), status: http.StatusForbidden, alt: http.StatusBadRequest},
		)
	}
	w.assertGrouped(t, rows)
}
