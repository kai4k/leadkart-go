//go:build integration

// arch-test:no-timeout-needed — newE2EFixture* → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:no-synctest — the cascade test crosses the real forwarder, router and
// Postgres; a virtual clock cannot order them.

// Hierarchy round 5, commit 1 (ADR 0069): editing a role is acting on everyone
// who holds it. A role's level need not match its holders' ranks, so the ROLE
// checks alone let a manager confer permissions on, or strip them from, peers
// and seniors, and let a member make a time-bound permission permanent for
// themself. Every scenario runs through the real mux.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) customRole(t *testing.T, token, name string, level int) string {
	t.Helper()
	r, id := f.createRoleVia(t, token, ports.CreateRoleRequest{Name: name, HierarchyLevel: level})
	if r.status != http.StatusCreated {
		t.Fatalf("create role %s: %d %s", name, r.status, r.body)
	}
	return id
}

func (f e2eFixture) grantRolePerm(t *testing.T, token, roleID, perm string) httpResp {
	t.Helper()
	return f.roleWrite(t, http.MethodPost, "/api/v1/roles/"+roleID+"/permissions/grant", token, ports.RolePermissionRequest{Permission: perm})
}

func (f e2eFixture) setManager(t *testing.T, token, memberID, managerID string) {
	t.Helper()
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+memberID+"/manager", token, ports.AssignUserManagerRequest{ManagerID: managerID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager of %s: %d %s", memberID, r.status, r.body)
	}
}

// TestE2E_RoleEdits_ReLevelCannotInvertAReportingLine: the platform operator re-levels
// a custom executive role from 50 to 16; its holders would then outrank their
// own Sales Manager.
func TestE2E_RoleEdits_ReLevelCannotInvertAReportingLine(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "relevel")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }

	sm := f.createMember(t, owner, "salesmgr")
	f.assignRole(t, owner.AccessToken, sm.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	exec := f.createMember(t, owner, "exec")
	custom := f.customRole(t, owner.AccessToken, "Exec2", 50)
	f.assignRole(t, owner.AccessToken, exec.membershipID, custom)
	f.setManager(t, owner.AccessToken, exec.membershipID, sm.membershipID)

	lvl := 16
	r := f.roleWrite(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{HierarchyLevel: &lvl})
	f.wantForbidden(t, "re-level 50 -> 16 above the holder's manager (rank 20)", r, http.StatusUnprocessableEntity, "role_level_breaks_reporting")
	if got := f.roleDto(t, owner.AccessToken, custom).HierarchyLevel; got != 50 {
		t.Errorf("the level changed to %d although the call was refused", got)
	}

	// A re-level that keeps every reporting line strictly ordered still works.
	ok := 30
	if r := f.roleWrite(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{HierarchyLevel: &ok}); r.status != http.StatusNoContent {
		t.Errorf("re-level 50 -> 30 (still junior to the manager at 20): %d %s, want 204", r.status, r.body)
	}
}

// TestE2E_RoleEdits_EndHoldersStaleTokens: a permission removed from a role (or
// a re-level) rotates every active holder's stamp in the same transaction, so a
// holder's old token answers 401 stale_token on another module's route, the
// refresh succeeds, and the permission is gone.
func TestE2E_RoleEdits_EndHoldersStaleTokens(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	owner := f.registerAndLogin(t, "rolestamp")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	const perm = "inventory.catalog.read"

	m := f.createMember(t, owner, "holder")
	f.assignRole(t, owner.AccessToken, m.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	custom := f.customRole(t, owner.AccessToken, "Catalogue readers", 60)
	if r := f.grantRolePerm(t, owner.AccessToken, custom, perm); r.status != http.StatusNoContent {
		t.Fatalf("grant %s to the custom role: %d %s", perm, r.status, r.body)
	}
	f.assignRole(t, owner.AccessToken, m.membershipID, custom)
	pair := f.loginPair(t, m.email, m.password)
	products := func(token string) int {
		return f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products", token, nil).status
	}
	if s := products(pair.AccessToken); s != http.StatusOK {
		t.Fatalf("sanity: the holder reads the catalogue through the role: %d", s)
	}
	f.warmStamp(t, pair.AccessToken)

	if r := f.roleWrite(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/revoke", owner.AccessToken, ports.RolePermissionRequest{Permission: perm}); r.status != http.StatusNoContent {
		t.Fatalf("owner revokes %s from the role: %d %s", perm, r.status, r.body)
	}
	if s := f.untilStale(t, pair.AccessToken); s.status != http.StatusUnauthorized || decodeError(t, s.body).Error != "stale_token" {
		t.Fatalf("holder's old token after the role lost a permission: %d %s, want 401 stale_token", s.status, s.body)
	}
	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	if rr.status != http.StatusOK {
		t.Fatalf("refresh: %d %s, want 200 (the session survives)", rr.status, rr.body)
	}
	var fresh ports.LoginResponse
	if err := json.Unmarshal(rr.body, &fresh); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if got := products(fresh.AccessToken); got != http.StatusForbidden {
		t.Errorf("refreshed token reads the catalogue: %d, want 403", got)
	}
	if perms, _ := tokenClaims(t, fresh.AccessToken)["permission"].([]any); slices.Contains(perms, any(perm)) {
		t.Errorf("refreshed token still carries %s", perm)
	}

	// A re-level ends the stale token too.
	f.evictStamp(t, m.personID)
	pair = f.loginPair(t, m.email, m.password)
	f.warmStamp(t, pair.AccessToken)
	lvl := 55
	if r := f.roleWrite(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{HierarchyLevel: &lvl}); r.status != http.StatusNoContent {
		t.Fatalf("owner re-levels the role: %d %s", r.status, r.body)
	}
	if s := f.untilStale(t, pair.AccessToken); s.status != http.StatusUnauthorized {
		t.Errorf("holder's old token after a re-level: %d %s, want 401 stale_token", s.status, s.body)
	}

	// A rename-only edit rotates nothing.
	f.evictStamp(t, m.personID)
	pair = f.loginPair(t, m.email, m.password)
	f.warmStamp(t, pair.AccessToken)
	if r := f.roleWrite(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{Name: "Catalogue readers v2"}); r.status != http.StatusNoContent {
		t.Fatalf("owner renames the role: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", pair.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("a rename ended the holder's token: %d %s, want 200", r.status, r.body)
	}
}

// Owner ruling 2026-11-25 (ADR 0073 addendum): creating, editing and deleting a role is LeadKart platform
// authority, so a company manager never reaches the role-edit commands. The rank, ceiling and holder rules
// those scenarios exercised are decided in the authority policy (role_management_test, authority unit tests)
// and still bind the platform actor; the route gate refusal is proved in platform_role_writes_e2e_test.go.
