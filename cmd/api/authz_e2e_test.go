//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Authorization end-to-end tests (ADR 0036 / ADR 0068): a plain member must
// not reach identity management routes or escalate; the CompanyOwner manages
// through the tenant.admin implication; nobody may modify themselves.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) createMemberAndLogin(t *testing.T, owner registeredTenant, prefix string) (string, string) {
	t.Helper()
	id := ids.NewV7().String()
	suffix := id[len(id)-8:]
	email := prefix + "-" + suffix + "@e2e.test"
	password := "Member-Pass-2026!" + suffix
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users", owner.AccessToken,
		ports.CreateUserRequest{Email: email, Password: password, FirstName: prefix, LastName: "Member"})
	if resp.status != http.StatusCreated {
		t.Fatalf("create member: %d %s", resp.status, resp.body)
	}
	var cu ports.CreateUserResponse
	if err := json.Unmarshal(resp.body, &cu); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cu.MembershipID, f.loginToken(t, email, password)
}

func (f e2eFixture) loginToken(t *testing.T, email, password string) string {
	t.Helper()
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: email, Password: password, DeviceLabel: "e2e"})
	if resp.status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, resp.status, resp.body)
	}
	var lr ports.LoginResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return lr.AccessToken
}

func (f e2eFixture) roleIDByName(t *testing.T, token, name string) string {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/roles", token, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list roles: %d %s", resp.status, resp.body)
	}
	var lr ports.ListRolesResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range lr.Roles {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("role %q not found", name)
	return ""
}

func TestE2E_Authz_MemberCannotEscalateButCanSelfServe(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "authz")
	memberID, memberTok := f.createMemberAndLogin(t, owner, "member")
	ownerRole := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)

	denied := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/users", nil},
		{http.MethodGet, "/api/v1/roles", nil},
		{http.MethodPost, "/api/v1/users/" + memberID + "/roles", ports.AssignUserRoleRequest{RoleID: ownerRole}},
		{http.MethodPatch, "/api/v1/users/" + memberID + "/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{"tenant.admin"}, Revoked: []string{}}},
		{http.MethodPost, "/api/v1/roles", ports.CreateRoleRequest{Name: "Escalator", HierarchyLevel: 50}},
		{http.MethodPost, "/api/v1/roles/" + ownerRole + "/permissions/grant", ports.RolePermissionRequest{Permission: "identity.users.view"}},
		{http.MethodGet, "/api/v1/tenants/" + owner.TenantID, nil},
		{http.MethodGet, "/api/v1/tenants/" + owner.TenantID + "/audit/events", nil},
	}
	for _, d := range denied {
		if resp := f.authedJSON(t, d.method, d.path, memberTok, d.body); resp.status != http.StatusForbidden {
			t.Errorf("member %s %s: %d %s, want 403", d.method, d.path, resp.status, resp.body)
		}
	}
	if resp := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+memberID, memberTok, nil); resp.status != http.StatusOK {
		t.Errorf("self read: %d %s", resp.status, resp.body)
	}
	if resp := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+memberID+"/profile", memberTok,
		ports.UpdateUserProfileRequest{Designation: "Rep"}); resp.status != http.StatusNoContent {
		t.Errorf("self profile: %d %s", resp.status, resp.body)
	}
}

func TestE2E_Authz_CompanyOwnerManagesViaTenantAdmin(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "owner")
	memberID, _ := f.createMemberAndLogin(t, owner, "staff")
	for _, path := range []string{"/api/v1/users", "/api/v1/roles", "/api/v1/tenants/" + owner.TenantID} {
		if resp := f.authedJSON(t, http.MethodGet, path, owner.AccessToken, nil); resp.status != http.StatusOK {
			t.Errorf("owner GET %s: %d %s", path, resp.status, resp.body)
		}
	}
	salesExec := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+memberID+"/roles", owner.AccessToken,
		ports.AssignUserRoleRequest{RoleID: salesExec}); resp.status != http.StatusNoContent {
		t.Errorf("owner assigns role: %d %s", resp.status, resp.body)
	}
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+owner.MembershipID+"/deactivate", owner.AccessToken,
		ports.DeactivateUserRequest{Reason: "self lockout attempt"})
	if resp.status != http.StatusForbidden || decodeError(t, resp.body).Error != "self_modification_forbidden" {
		t.Errorf("owner self-deactivate: %d %s", resp.status, resp.body)
	}
}
