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

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
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
	return f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+roleID+"/permissions/grant", token, ports.RolePermissionRequest{Permission: perm})
}

func (f e2eFixture) setManager(t *testing.T, token, memberID, managerID string) {
	t.Helper()
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+memberID+"/manager", token, ports.AssignUserManagerRequest{ManagerID: managerID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager of %s: %d %s", memberID, r.status, r.body)
	}
}

// TestE2E_RoleEdits_ActOnEveryHolder: manager M1 (rank 20, holds identity.roles.update
// and the permission P) edits a custom level-30 role that PEER manager M2 also
// holds. M1 can neither change M2's overrides nor assign M2 a role, so the role
// edit must not do either by the back door.
func TestE2E_RoleEdits_ActOnEveryHolder(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "holders")
	p := permission.IdentityPermissions
	perm := p.Tasks.WorkItems.Reassign
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }

	m1 := f.createMember(t, owner, "m1")
	f.assignRole(t, owner.AccessToken, m1.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	f.grantOverrides(t, owner.AccessToken, m1.membershipID, []string{p.Roles.Update, p.Roles.Manage, perm}, nil)
	m2 := f.createMember(t, owner, "m2")
	f.assignRole(t, owner.AccessToken, m2.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	custom := f.customRole(t, owner.AccessToken, "Shared custom", 30)
	f.assignRole(t, owner.AccessToken, m2.membershipID, custom)
	m1Tok := f.loginToken(t, m1.email, m1.password)

	rolePerms := func() []string { return f.roleDto(t, owner.AccessToken, custom).Permissions }

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a: conferring a permission on a role a peer holds is refused", func(t *testing.T) {
		r := f.grantRolePerm(t, m1Tok, custom, perm)
		f.wantForbidden(t, "M1 grants P to a role peer M2 holds", r, http.StatusForbidden, "insufficient_rank")
		if slices.Contains(rolePerms(), perm) {
			t.Errorf("the role gained %s although the call was refused", perm)
		}
	})
	// The owner puts P on the role: M2 holds it through the role.
	if r := f.grantRolePerm(t, owner.AccessToken, custom, perm); r.status != http.StatusNoContent {
		t.Fatalf("owner grants P: %d %s", r.status, r.body)
	}
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("c: stripping a permission from a role a peer holds is refused (revoke, replace)", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/revoke", m1Tok, ports.RolePermissionRequest{Permission: perm})
		f.wantForbidden(t, "revoke", r, http.StatusForbidden, "insufficient_rank")
		r = f.authedJSON(t, http.MethodPut, "/api/v1/roles/"+custom+"/permissions", m1Tok, ports.ReplaceRolePermissionsRequest{Permissions: []string{}})
		f.wantForbidden(t, "replace with []", r, http.StatusForbidden, "insufficient_rank")
		if !slices.Contains(rolePerms(), perm) {
			t.Errorf("the role lost %s although the calls were refused", perm)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivate: authority is answered before role_in_use", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/deactivate", m1Tok, nil)
		f.wantForbidden(t, "deactivate a role a peer holds", r, http.StatusForbidden, "insufficient_rank")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("re-level: a role a peer holds cannot be re-levelled either", func(t *testing.T) {
		lvl := 40
		r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, m1Tok, ports.UpdateRoleRequest{HierarchyLevel: &lvl})
		f.wantForbidden(t, "re-level", r, http.StatusForbidden, "insufficient_rank")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("rename only is not acting on holders", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, m1Tok, ports.UpdateRoleRequest{Name: "Shared custom renamed"})
		if r.status != http.StatusNoContent {
			t.Errorf("rename by M1: %d %s, want 204", r.status, r.body)
		}
	})
}

// TestE2E_RoleEdits_SelfConferralIsRefused: A holds P only through a time-bound
// approved override and also holds a role below their rank; A must not make P
// permanent for themself by granting it to that role.
func TestE2E_RoleEdits_SelfConferralIsRefused(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "selfconf")
	p := permission.IdentityPermissions
	perm := p.Tasks.WorkItems.Reassign
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }

	tl := f.createMember(t, owner, "tl")
	f.assignRole(t, owner.AccessToken, tl.membershipID, roleID(role.SystemRoles.Tenant.TeamLead))
	a := f.createMember(t, owner, "a")
	f.assignRole(t, owner.AccessToken, a.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	below := f.customRole(t, owner.AccessToken, "Below A", 60)
	f.assignRole(t, owner.AccessToken, a.membershipID, below)
	f.setManager(t, owner.AccessToken, a.membershipID, tl.membershipID)
	f.grantOverrides(t, owner.AccessToken, a.membershipID, []string{p.Roles.Update}, nil)

	aTok := f.loginToken(t, a.email, a.password)
	sub, reqID := f.requestElevation(t, aTok, perm)
	if sub.status != http.StatusCreated {
		t.Fatalf("A requests P: %d %s", sub.status, sub.body)
	}
	if ap := f.approveRequest(t, f.loginToken(t, tl.email, tl.password), reqID); ap.status != http.StatusNoContent {
		t.Fatalf("T approves P for A: %d %s", ap.status, ap.body)
	}
	f.evictStamp(t, a.personID)
	aTok = f.loginToken(t, a.email, a.password)

	r := f.grantRolePerm(t, aTok, below, perm)
	f.wantForbidden(t, "A grants its time-bound P to a role A holds", r, http.StatusForbidden, "self_modification_forbidden")
	if slices.Contains(f.roleDto(t, owner.AccessToken, below).Permissions, perm) {
		t.Errorf("P is now permanent for A through the role they hold")
	}
}

// TestE2E_RoleEdits_ReLevelCannotInvertAReportingLine: a Senior Manager re-levels
// a custom executive role from 50 to 16; its holders would then outrank their
// own Sales Manager.
func TestE2E_RoleEdits_ReLevelCannotInvertAReportingLine(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "relevel")
	p := permission.IdentityPermissions
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }

	senior := f.createMember(t, owner, "senior")
	f.assignRole(t, owner.AccessToken, senior.membershipID, roleID(role.SystemRoles.Tenant.SeniorManager))
	f.grantOverrides(t, owner.AccessToken, senior.membershipID, []string{p.Roles.Update}, nil)
	sm := f.createMember(t, owner, "salesmgr")
	f.assignRole(t, owner.AccessToken, sm.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	exec := f.createMember(t, owner, "exec")
	custom := f.customRole(t, owner.AccessToken, "Exec2", 50)
	f.assignRole(t, owner.AccessToken, exec.membershipID, custom)
	f.setManager(t, owner.AccessToken, exec.membershipID, sm.membershipID)
	sTok := f.loginToken(t, senior.email, senior.password)

	lvl := 16
	r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, sTok, ports.UpdateRoleRequest{HierarchyLevel: &lvl})
	f.wantForbidden(t, "re-level 50 -> 16 above the holder's manager (rank 20)", r, http.StatusUnprocessableEntity, "role_level_breaks_reporting")
	if got := f.roleDto(t, owner.AccessToken, custom).HierarchyLevel; got != 50 {
		t.Errorf("the level changed to %d although the call was refused", got)
	}

	// A re-level that keeps every reporting line strictly ordered still works.
	ok := 30
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, sTok, ports.UpdateRoleRequest{HierarchyLevel: &ok}); r.status != http.StatusNoContent {
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

	if r := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+custom+"/permissions/revoke", owner.AccessToken, ports.RolePermissionRequest{Permission: perm}); r.status != http.StatusNoContent {
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
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{HierarchyLevel: &lvl}); r.status != http.StatusNoContent {
		t.Fatalf("owner re-levels the role: %d %s", r.status, r.body)
	}
	if s := f.untilStale(t, pair.AccessToken); s.status != http.StatusUnauthorized {
		t.Errorf("holder's old token after a re-level: %d %s, want 401 stale_token", s.status, s.body)
	}

	// A rename-only edit rotates nothing.
	f.evictStamp(t, m.personID)
	pair = f.loginPair(t, m.email, m.password)
	f.warmStamp(t, pair.AccessToken)
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/roles/"+custom, owner.AccessToken, ports.UpdateRoleRequest{Name: "Catalogue readers v2"}); r.status != http.StatusNoContent {
		t.Fatalf("owner renames the role: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", pair.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("a rename ended the holder's token: %d %s, want 200", r.status, r.body)
	}
}

// TestE2E_RoleReactivate_NeedsADurableHold (review G2j B1): reactivating an inactive role gives its
// permissions back to its holders, which is a conferral. An actor who holds P only through a
// time-bound approved override cannot make P durable for others that way, exactly as they cannot
// by assigning a role that carries P; the owner can.
func TestE2E_RoleReactivate_NeedsADurableHold(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "reactconf")
	p := permission.IdentityPermissions
	perm := p.Tasks.WorkItems.Reassign
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }

	sm := f.createMember(t, owner, "teamlead")
	f.assignRole(t, owner.AccessToken, sm.membershipID, roleID(role.SystemRoles.Tenant.TeamLead))
	a := f.createMember(t, owner, "a")
	f.assignRole(t, owner.AccessToken, a.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	f.setManager(t, owner.AccessToken, a.membershipID, sm.membershipID)
	f.grantOverrides(t, owner.AccessToken, a.membershipID, []string{p.Roles.Update, p.Roles.Manage}, nil)

	// R carries P and is held by H, whose membership is inactive when R goes inactive.
	h := f.createMember(t, owner, "holder")
	r := f.customRole(t, owner.AccessToken, "Carries P", 60)
	if g := f.grantRolePerm(t, owner.AccessToken, r, perm); g.status != http.StatusNoContent {
		t.Fatalf("grant P to R: %d %s", g.status, g.body)
	}
	f.assignRole(t, owner.AccessToken, h.membershipID, r)
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+h.membershipID+"/deactivate", owner.AccessToken, ports.DeactivateUserRequest{Reason: "reactivate conferral"}); d.status != http.StatusNoContent {
		t.Fatalf("deactivate the holder: %d %s", d.status, d.body)
	}
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+r+"/deactivate", owner.AccessToken, nil); d.status != http.StatusOK {
		t.Fatalf("deactivate R: %d %s", d.status, d.body)
	}
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+h.membershipID+"/reactivate", owner.AccessToken, nil); d.status != http.StatusNoContent {
		t.Fatalf("reactivate the holder: %d %s", d.status, d.body)
	}

	// A holds P only until the approval expires.
	aTok := f.loginToken(t, a.email, a.password)
	sub, reqID := f.requestElevation(t, aTok, perm)
	if sub.status != http.StatusCreated {
		t.Fatalf("A requests P: %d %s", sub.status, sub.body)
	}
	if ap := f.approveRequest(t, f.loginToken(t, sm.email, sm.password), reqID); ap.status != http.StatusNoContent {
		t.Fatalf("the manager approves P for A: %d %s", ap.status, ap.body)
	}
	f.evictStamp(t, a.personID)
	aTok = f.loginToken(t, a.email, a.password)

	res := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+r+"/reactivate", aTok, nil)
	f.wantForbidden(t, "A reactivates a role carrying its time-bound P", res, http.StatusForbidden, "grant_exceeds_authority")
	if f.roleDto(t, owner.AccessToken, r).IsActive {
		t.Fatal("the role was reactivated by a refused call")
	}
	if got := f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+r+"/reactivate", owner.AccessToken, nil); got.status != http.StatusOK {
		t.Fatalf("the owner (durable hold) reactivates R: %d %s", got.status, got.body)
	}
}
