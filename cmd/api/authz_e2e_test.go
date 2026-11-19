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
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) createMemberAndLogin(t *testing.T, owner registeredTenant, prefix string) (string, string) {
	t.Helper()
	m := f.createMember(t, owner, prefix)
	return m.membershipID, f.loginToken(t, m.email, m.password)
}

// createdMember is a member the owner created over HTTP, with the
// credentials needed to log in again after its roles change.
type createdMember struct{ membershipID, personID, email, password string }

func (f e2eFixture) createMember(t *testing.T, owner registeredTenant, prefix string) createdMember {
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
	return createdMember{membershipID: cu.MembershipID, personID: cu.PersonID, email: email, password: password}
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

// The self-target gate compares ids as UUID values: spelling the caller's own
// membership id differently (upper case, urn prefix, no hyphens) must not
// bypass it, even though the handler/repository normalise the same spelling.
func TestE2E_Authz_SelfTargetCannotBeBypassedBySpelling(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "selfspell")
	salesExec := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	before := f.getUser(t, owner.AccessToken, owner.MembershipID)
	spellings := map[string]string{
		"uppercase":  strings.ToUpper(owner.MembershipID),
		"urn":        "urn:uuid:" + owner.MembershipID,
		"hyphenless": strings.ReplaceAll(owner.MembershipID, "-", ""),
		"braces":     "%7B" + owner.MembershipID + "%7D",
	}
	for name, own := range spellings {
		deact := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+own+"/deactivate", owner.AccessToken,
			ports.DeactivateUserRequest{Reason: "self lockout attempt"})
		if deact.status != http.StatusForbidden || decodeError(t, deact.body).Error != "self_modification_forbidden" {
			t.Errorf("%s deactivate: %d %s, want 403 self_modification_forbidden", name, deact.status, deact.body)
		}
		assign := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+own+"/roles", owner.AccessToken,
			ports.AssignUserRoleRequest{RoleID: salesExec})
		if assign.status != http.StatusForbidden || decodeError(t, assign.body).Error != "self_modification_forbidden" {
			t.Errorf("%s assign role: %d %s, want 403 self_modification_forbidden", name, assign.status, assign.body)
		}
	}
	// The rejections must also have had no effect: still active, same roles.
	after := f.getUser(t, owner.AccessToken, owner.MembershipID)
	if after.Status != before.Status || after.Status != "active" {
		t.Errorf("owner status = %q (was %q), want unchanged \"active\"", after.Status, before.Status)
	}
	if !slices.Equal(after.RoleIDs, before.RoleIDs) {
		t.Errorf("owner role set changed: %v -> %v", before.RoleIDs, after.RoleIDs)
	}
}

func (f e2eFixture) getUser(t *testing.T, token, id string) ports.UserDto {
	t.Helper()
	resp := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+id, token, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("get user %s: %d %s", id, resp.status, resp.body)
	}
	var u ports.UserDto
	if err := json.Unmarshal(resp.body, &u); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	return u
}

// Ids in the request BODY are compared as UUID values too: spelling a
// member's own id differently as manager_id must be rejected exactly like the
// canonical self-manager attempt, and re-assigning an already-assigned role
// through another spelling of role_id must behave exactly like the canonical
// repeat (no error, no duplicate assignment).
func TestE2E_Authz_BodyIDSpellingCannotBypassDomainRules(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "bodyspell")
	memberID, _ := f.createMemberAndLogin(t, owner, "bodymember")
	salesExec := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive)

	managerPath := "/api/v1/users/" + memberID + "/manager"
	canon := f.authedJSON(t, http.MethodPut, managerPath, owner.AccessToken, ports.AssignUserManagerRequest{ManagerID: memberID})
	if canon.status != http.StatusUnprocessableEntity {
		t.Fatalf("canonical self-manager: %d %s, want 422", canon.status, canon.body)
	}
	for name, mgr := range map[string]string{
		"uppercase":  strings.ToUpper(memberID),
		"urn":        "urn:uuid:" + memberID,
		"hyphenless": strings.ReplaceAll(memberID, "-", ""),
		"braces":     "{" + memberID + "}",
	} {
		resp := f.authedJSON(t, http.MethodPut, managerPath, owner.AccessToken, ports.AssignUserManagerRequest{ManagerID: mgr})
		if resp.status != canon.status || decodeErrorCode(t, resp.body) != decodeErrorCode(t, canon.body) {
			t.Errorf("%s self-manager: %d %s, want same as canonical %d %s", name, resp.status, resp.body, canon.status, canon.body)
		}
	}
	if u := f.getUser(t, owner.AccessToken, memberID); u.ReportsTo != "" {
		t.Errorf("member reports_to = %q after rejected attempts, want empty", u.ReportsTo)
	}

	rolesPath := "/api/v1/users/" + memberID + "/roles"
	first := f.authedJSON(t, http.MethodPost, rolesPath, owner.AccessToken, ports.AssignUserRoleRequest{RoleID: salesExec})
	repeat := f.authedJSON(t, http.MethodPost, rolesPath, owner.AccessToken, ports.AssignUserRoleRequest{RoleID: salesExec})
	if first.status != http.StatusNoContent || repeat.status != first.status {
		t.Fatalf("canonical assign/repeat: %d / %d", first.status, repeat.status)
	}
	for name, rid := range map[string]string{
		"uppercase":  strings.ToUpper(salesExec),
		"urn":        "urn:uuid:" + salesExec,
		"hyphenless": strings.ReplaceAll(salesExec, "-", ""),
	} {
		resp := f.authedJSON(t, http.MethodPost, rolesPath, owner.AccessToken, ports.AssignUserRoleRequest{RoleID: rid})
		if resp.status != repeat.status {
			t.Errorf("%s repeat assign: %d %s, want %d like the canonical repeat", name, resp.status, resp.body, repeat.status)
		}
	}
	got := f.getUser(t, owner.AccessToken, memberID)
	n := 0
	for _, r := range got.RoleIDs {
		if r == salesExec {
			n++
		}
	}
	if n != 1 {
		t.Errorf("role %s assigned %d times, want exactly once (role_ids=%v)", salesExec, n, got.RoleIDs)
	}
}
