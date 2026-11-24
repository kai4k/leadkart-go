//go:build integration

// arch-test:no-timeout-needed — helpers and one test over newE2EFixture, which bounds its own startup;
// per-request HTTP uses t.Context().

// Owner ruling 2026-11-25 (ADR 0073 addendum): creating, editing and deleting a role is LeadKart platform
// authority. No company member holds identity.roles.{create,update,manage}, the owner and tenant.admin
// included. The role scenarios in this package test what a role write DOES (names, levels, permissions,
// holders, the delete rule), so by default a role write made with a company token is sent the way the
// platform reaches a company: as a platform operator holding platform.roles.manage with the company in
// X-Tenant-Id. A test that means the company caller (to prove the refusal) passes [asCompanyHdr].

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// asCompanyHdr marks a request that must reach the server exactly as the company caller sent it.
var asCompanyHdr = map[string]string{"X-E2E-As-Company": "1"}

var platformRoleWrite = regexp.MustCompile(`^/api/v1/roles(/[^/?]+(/(permissions(/grant|/revoke)?|parent|deactivate|reactivate))?)?(\?.*)?$`)

func isPlatformRoleWrite(method, path string) bool {
	if method == http.MethodGet || method == http.MethodHead || !platformRoleWrite.MatchString(path) {
		return false
	}
	return true
}

// tokenFacts reads the claims a test token carries (no verification: the server does that).
func tokenFacts(token string) (tenantID string, platform, impersonating bool) {
	tenantID, platform, impersonating, _ = roleTokenClaims(token)
	return tenantID, platform, impersonating
}

// tokenClaims is [tokenFacts] plus the permissions the token carries.
func roleTokenClaims(token string) (tenantID string, platform, impersonating bool, perms []string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false, false, nil
	}
	var c struct {
		Permissions []string        `json:"permission"`
		TenantID    string          `json:"tenant_id"`
		IsPlatform  bool            `json:"is_platform"`
		Act         json.RawMessage `json:"act"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return "", false, false, nil
	}
	return c.TenantID, c.IsPlatform, len(c.Act) > 0 && string(c.Act) != "null", c.Permissions
}

var (
	roleOperatorMu    sync.Mutex
	roleOperatorToken = map[string]string{}
)

// asPlatformForRoleWrite swaps a company token for the platform operator that reaches that company.
func (f e2eFixture) asPlatformForRoleWrite(t *testing.T, method, path, token string, headers map[string]string) (string, map[string]string) {
	t.Helper()
	if !isPlatformRoleWrite(method, path) || headers["X-E2E-As-Company"] != "" || headers["X-Tenant-Id"] != "" || token == "" {
		return token, headers
	}
	tenantID, platform, impersonating, perms := roleTokenClaims(token)
	if tenantID == "" || platform || impersonating {
		return token, headers
	}
	// Only a caller the route gate used to let through is written as the platform: one holding
	// tenant.admin or an explicit role-management grant. A plain member keeps its own refusal.
	p := permission.IdentityPermissions
	if !slices.ContainsFunc(perms, func(n string) bool {
		return n == p.Meta.TenantAdmin || permission.IsPlatformManagedRole(n)
	}) {
		return token, headers
	}
	roleOperatorMu.Lock()
	op, ok := roleOperatorToken[f.URL]
	if !ok {
		p := permission.IdentityPermissions
		op = mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.RolesView, p.Platform.RolesManage}, false)
		roleOperatorToken[f.URL] = op
	}
	roleOperatorMu.Unlock()
	out := map[string]string{"X-Tenant-Id": tenantID}
	for k, v := range headers {
		out[k] = v
	}
	return op, out
}

// roleWrite makes a role write the way the platform reaches a company (see [asPlatformForRoleWrite]).
// Scenario set-up uses it; a test that means the company caller sends the request itself and gets the
// route gate's 403.
func (f e2eFixture) roleWrite(t *testing.T, method, path, token string, body any) httpResp {
	t.Helper()
	op, hdr := f.asPlatformForRoleWrite(t, method, path, token, nil)
	return f.doWithHeaders(t, method, path, op, body, hdr)
}

// callRoleWrite is [attackWorld.call] for a role write: the racer is the platform operator that reaches
// the company, resolved up front (the swap needs the test handle).
func (w *attackWorld) callRoleWrite(t *testing.T, method, path, tok string, body any) reqFn {
	t.Helper()
	opTok, hdr := w.f.asPlatformForRoleWrite(t, method, path, tok, nil)
	return func(ctx context.Context) (httpResp, error) { return w.f.tryDo(ctx, method, path, opTok, body, hdr) }
}

// A company never creates, edits or deletes its roles (owner ruling 2026-11-25): the owner and a
// company administrator are refused on every role write, while assigning an existing role stays a
// company power and a platform operator may write.
func TestE2E_RoleWrites_AreLeadKartPlatformAuthority(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "roleplatform")
	tok := owner.AccessToken
	base := "/api/v1/roles"
	role := f.customRole(t, tok, "Reachable Role", 60) // written by the platform operator

	admin := f.createMember(t, owner, "company-admin")
	f.assignRole(t, tok, admin.membershipID, f.roleIDByName(t, tok, "Administrator"))
	adminTok := f.loginToken(t, admin.email, admin.password)

	own := map[string]string{"X-E2E-As-Company": "1"}
	for name, token := range map[string]string{"owner": tok, "company administrator": adminTok} {
		for _, rt := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, base, ports.CreateRoleRequest{Name: "Nope", HierarchyLevel: 60}},
			{http.MethodPatch, base + "/" + role, ports.UpdateRoleRequest{Name: "Renamed"}},
			{http.MethodPost, base + "/" + role + "/permissions/grant", ports.RolePermissionRequest{Permission: permission.IdentityPermissions.Crm.Leads.Read}},
			{http.MethodPost, base + "/" + role + "/deactivate", nil},
			{http.MethodPost, base + "/" + role + "/reactivate", nil},
		} {
			if r := f.doWithHeaders(t, rt.method, rt.path, token, rt.body, own); r.status != http.StatusForbidden {
				t.Errorf("%s %s %s: %d %s, want 403", name, rt.method, rt.path, r.status, r.body)
			}
		}
		// Reading stays a company power, and so does assigning an existing role.
		if r := f.doWithHeaders(t, http.MethodGet, base+"/"+role, token, nil, own); r.status != http.StatusOK {
			t.Errorf("%s reads a role: %d %s", name, r.status, r.body)
		}
	}
	// The platform operator wrote the role above; the role is unchanged by the refused attempts.
	if got := f.roleDto(t, tok, role); got.Name != "Reachable Role" || !got.IsActive {
		t.Fatalf("a refused role write changed the role: %+v", got)
	}
	member := f.createMember(t, owner, "assignee")
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+member.membershipID+"/roles", adminTok, ports.AssignUserRoleRequest{RoleID: role}, own); r.status != http.StatusNoContent {
		t.Fatalf("a company administrator assigns an existing role: %d %s, want 204", r.status, r.body)
	}
	// effective_permissions no longer lists the three names for the owner, and keeps assign.
	eff := f.effectivePermissions(t, tok)
	for _, name := range []string{permission.IdentityPermissions.Roles.Create, permission.IdentityPermissions.Roles.Update, permission.IdentityPermissions.Roles.Manage} {
		for _, e := range eff {
			if e == name {
				t.Errorf("owner effective_permissions lists %s", name)
			}
		}
	}
}
