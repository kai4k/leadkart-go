//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// The escalation chain behind Tasks A5/A6 (ADR 0069), walked through the real
// mux. A Team Lead approves a permission they do not hold for their report; the
// report then edits a SYSTEM role. The chain must die at BOTH ends: the approval
// (A6) and the role command (A5).

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

type chainWorld struct {
	f         e2eFixture
	owner     registeredTenant
	ownerRole string
	team      createdMember // Team Lead T
	exec      createdMember // Sales Executive E, reports to T
	tTok      string
}

func newChainWorld(t *testing.T, prefix string) chainWorld {
	t.Helper()
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, prefix)
	w := chainWorld{f: f, owner: owner, ownerRole: f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)}
	w.team = f.createMember(t, owner, "tl")
	w.exec = f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, w.team.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.TeamLead))
	f.assignRole(t, owner.AccessToken, w.exec.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+w.exec.membershipID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: w.team.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager: %d %s", r.status, r.body)
	}
	w.tTok = f.loginToken(t, w.team.email, w.team.password)
	return w
}

func (w chainWorld) getRole(t *testing.T, id string) ports.RoleDto {
	t.Helper()
	r := w.f.authedJSON(t, http.MethodGet, "/api/v1/roles/"+id, w.owner.AccessToken, nil)
	var dto ports.RoleDto
	if r.status != http.StatusOK || json.Unmarshal(r.body, &dto) != nil {
		t.Fatalf("get role %s: %d %s", id, r.status, r.body)
	}
	return dto
}

// walk runs steps 2-3 (request, approval) and returns the approval answer and
// the executive's fresh token (valid when the approval succeeded).
func (w chainWorld) walk(t *testing.T) (httpResp, string) {
	t.Helper()
	eTok := w.f.loginToken(t, w.exec.email, w.exec.password)
	sub, reqID := w.f.requestElevation(t, eTok, permission.IdentityPermissions.Roles.Update)
	if sub.status != http.StatusCreated {
		t.Fatalf("step 2: E requests identity.roles.update: %d %s", sub.status, sub.body)
	}
	ap := w.f.approveRequest(t, w.tTok, reqID)
	t.Logf("step 3: Team Lead approves identity.roles.update for their report: %d %s", ap.status, ap.body)
	// The approval rotated E's stamp (their old token is stale by design); log in again.
	w.f.evictStamp(t, w.exec.personID)
	return ap, w.f.loginToken(t, w.exec.email, w.exec.password)
}

// TestE2E_RoleAuthorityChain_ClosedAtBothEnds: the approver lacks the
// permission (403 at step 3); with the permission hand-granted to the approver
// the approval is legitimate and the role command refuses (409 at step 4).
func TestE2E_RoleAuthorityChain_ClosedAtBothEnds(t *testing.T) {
	// arch-test:serial — each subtest builds its own fixture; they share nothing.
	t.Run("step 3: the approver does not hold the permission", func(t *testing.T) {
		w := newChainWorld(t, "chain3")
		ap, eTok := w.walk(t)
		if ap.status != http.StatusForbidden || decodeError(t, ap.body).Error != "grant_exceeds_authority" {
			t.Errorf("step 3 answered %d %s, want 403 grant_exceeds_authority", ap.status, ap.body)
		}
		if ap.status != http.StatusNoContent {
			return
		}
		// The chain continues only on a vulnerable build; report what it does.
		before := w.getRole(t, w.ownerRole)
		r := w.f.authedJSON(t, http.MethodPost, "/api/v1/roles/"+w.ownerRole+"/permissions/revoke", eTok,
			ports.RolePermissionRequest{Permission: permission.IdentityPermissions.Meta.TenantAdmin})
		after := w.getRole(t, w.ownerRole)
		t.Errorf("step 4 (revoke tenant.admin from Company Owner): %d %s; Company Owner permissions %d -> %d, tenant.admin kept: %v",
			r.status, r.body, len(before.Permissions), len(after.Permissions),
			slices.Contains(after.Permissions, permission.IdentityPermissions.Meta.TenantAdmin))
		r = w.f.authedJSON(t, http.MethodPut, "/api/v1/roles/"+w.ownerRole+"/permissions", eTok, map[string]any{})
		after = w.getRole(t, w.ownerRole)
		t.Errorf("variant PUT {}: %d %s; Company Owner permissions now %d", r.status, r.body, len(after.Permissions))
	})
	// arch-test:serial — each subtest builds its own fixture; they share nothing.
	t.Run("step 4: approval is legitimate but the system role is immutable", func(t *testing.T) {
		w := newChainWorld(t, "chain4")
		// The owner hands T the permission (an ordinary, allowed override).
		if r := w.f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+w.team.membershipID+"/permission-overrides", w.owner.AccessToken,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{permission.IdentityPermissions.Roles.Update}, Revoked: []string{}}); r.status != http.StatusNoContent {
			t.Fatalf("grant T identity.roles.update: %d %s", r.status, r.body)
		}
		w.tTok = w.f.loginToken(t, w.team.email, w.team.password)
		ap, eTok := w.walk(t)
		if ap.status != http.StatusNoContent {
			t.Fatalf("step 3 with the permission held: %d %s, want 204", ap.status, ap.body)
		}
		for _, c := range []struct {
			name, method, path string
			body               any
			status             int
			code               string
		}{
			{"revoke tenant.admin", http.MethodPost, "/permissions/revoke", ports.RolePermissionRequest{Permission: permission.IdentityPermissions.Meta.TenantAdmin}, 409, "role_system_default"},
			{"PUT {} (missing key)", http.MethodPut, "/permissions", map[string]any{}, 400, "invalid_body"},
			{"PUT []", http.MethodPut, "/permissions", ports.ReplaceRolePermissionsRequest{Permissions: []string{}}, 409, "role_system_default"},
			{"rename", http.MethodPatch, "", map[string]any{"name": "Pwned"}, 409, "role_system_default"},
		} {
			before := w.getRole(t, w.ownerRole)
			r := w.f.authedJSON(t, c.method, "/api/v1/roles/"+w.ownerRole+c.path, eTok, c.body)
			after := w.getRole(t, w.ownerRole)
			if r.status != c.status || decodeError(t, r.body).Error != c.code {
				t.Errorf("step 4 %s: %d %s, want %d %s (permissions %d -> %d)", c.name, r.status, r.body, c.status, c.code, len(before.Permissions), len(after.Permissions))
			}
			if !slices.Equal(before.Permissions, after.Permissions) || before.Name != after.Name {
				t.Errorf("step 4 %s changed the Company Owner role: %+v -> %+v", c.name, before, after)
			}
		}
	})
}
