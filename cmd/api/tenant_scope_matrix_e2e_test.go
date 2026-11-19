//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// X-Tenant-Id coverage matrix (ADR 0039 / ADR 0068 D7): non-super operators,
// tenant users, impersonation tokens and cross-module scoped reads/writes,
// end to end against real Postgres and the real mux.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// A NON-super operator holding only platform.users.manage / platform.roles.manage
// performs identity writes in the scoped tenant through the platform equivalents,
// but cannot assign a role that confers non-identity permissions (D4: no grant
// beyond your own authority).
//
// This documents TODAY's behaviour. The rank-hierarchy plan
// (docs/plans/2026-11-19-rank-hierarchy-and-grant-authority.md) redesigns grant
// authority; update the 403 expectation below when that plan lands.
func TestE2E_TenantScope_NonSuperOperatorIdentityWrites(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "scopenonsuper")
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{
		p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage, p.Platform.RolesView, p.Platform.RolesManage,
	}, false)

	created := f.doWithHeaders(t, http.MethodPost, "/api/v1/users", op,
		ports.CreateUserRequest{Email: "nonsuper-new@e2e.test", Password: "NonSuper-User-2026!", FirstName: "Non", LastName: "Super"}, scopeHdr(b.TenantID))
	if created.status != http.StatusCreated {
		t.Fatalf("non-super operator create user in B: %d %s", created.status, created.body)
	}
	var cu ports.CreateUserResponse
	if err := json.Unmarshal(created.body, &cu); err != nil {
		t.Fatalf("decode: %v", err)
	}
	roleResp := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", op, ports.CreateRoleRequest{Name: "Empty Role", HierarchyLevel: 50}, scopeHdr(b.TenantID))
	if roleResp.status != http.StatusCreated {
		t.Fatalf("non-super operator create role in B: %d %s", roleResp.status, roleResp.body)
	}
	emptyRoleID := f.roleIDByName(t, b.AccessToken, "Empty Role")

	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/roles", op,
		ports.AssignUserRoleRequest{RoleID: emptyRoleID}, scopeHdr(b.TenantID)); r.status != http.StatusNoContent {
		t.Errorf("assigning a role with no extra permissions: %d %s", r.status, r.body)
	}
	salesExec := f.roleIDByName(t, b.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/roles", op,
		ports.AssignUserRoleRequest{RoleID: salesExec}, scopeHdr(b.TenantID))
	if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "grant_exceeds_authority" {
		t.Errorf("assigning a role that holds non-identity permissions: %d %s", r.status, r.body)
	}
}

// Tenant registration needs platform.tenants.create on top of the platform
// anchor: a non-super operator without it is 403.
func TestE2E_TenantScope_RegisterTenantNeedsCreatePermission(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.TenantsManage, p.Platform.UsersManage}, false)
	r := f.do(t, http.MethodPost, "/api/v1/tenants", op, ports.RegisterTenantRequest{
		Slug: "no-create-perm", LegalName: "No Create Pvt Ltd", DisplayName: "No Create",
		AdminEmail: "admin@no-create.test", AdminPassword: "Correct-Horse-Battery-9!", AdminFirstName: "No", AdminLastName: "Create",
	})
	if r.status != http.StatusForbidden {
		t.Fatalf("operator without platform.tenants.create: %d %s", r.status, r.body)
	}
}

// A tenant user's header is ignored on a WRITE too: the role lands in their own
// tenant and never in the tenant the header names.
func TestE2E_TenantScope_TenantUserHeaderIgnoredOnWrite(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "ignwa")
	b := f.registerAndLogin(t, "ignwb")
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", b.AccessToken,
		ports.CreateRoleRequest{Name: "Header Ignored Role", HierarchyLevel: 50}, scopeHdr(a.TenantID))
	if r.status != http.StatusCreated {
		t.Fatalf("tenant user write with foreign header: %d %s", r.status, r.body)
	}
	var inB, inA bool
	for _, tok := range []struct {
		token string
		dst   *bool
	}{{b.AccessToken, &inB}, {a.AccessToken, &inA}} {
		lr := f.authedJSON(t, http.MethodGet, "/api/v1/roles", tok.token, nil)
		var roles ports.ListRolesResponse
		if err := json.Unmarshal(lr.body, &roles); err != nil {
			t.Fatalf("decode roles: %v", err)
		}
		*tok.dst = slices.ContainsFunc(roles.Roles, func(x ports.RoleDto) bool { return x.Name == "Header Ignored Role" })
	}
	if !inB || inA {
		t.Errorf("role must land in the caller's own tenant only: inB=%v inA=%v", inB, inA)
	}
}

// An impersonation token (act claim, aud=leadkart-impersonation) is never an
// operator for scope purposes: the header is ignored and the request stays in
// the session's tenant.
func TestE2E_TenantScope_ImpersonationTokenHeaderIgnored(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "impa")
	b := f.registerAndLogin(t, "impb")
	platformTok := f.mintPlatformToken(t, "")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok, ports.CreateImpersonationSessionRequest{
		TargetTenantID: a.TenantID, Reason: "ticket #4321 — scope header e2e", DurationMinutes: 30,
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.status, resp.body)
	}
	var session ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(resp.body, &session); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, hdr := range []string{b.TenantID, unknownTenantID, "not-a-uuid"} {
		r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", session.AccessToken, nil, scopeHdr(hdr))
		got := userIDs(t, r.body)
		if r.status != http.StatusOK || !slices.Contains(got, a.MembershipID) || slices.Contains(got, b.MembershipID) {
			t.Errorf("impersonation token with header %q must stay in tenant A: %d %s", hdr, r.status, r.body)
		}
	}
}

// Cross-module proof (orders): a scoped operator reads in the scoped tenant
// (writes are read-only under a foreign header, so tenant A's own admin creates
// the quotation); the data is invisible to the other tenant (RLS).
func TestE2E_TenantScope_OrdersScopedRead(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "scopeorda")
	b := f.registerAndLogin(t, "scopeordb")
	op := f.mintPlatformTokenWith(t, nil, true)

	created := f.authedJSON(t, http.MethodPost, "/api/v1/orders/quotations", a.AccessToken, ordersports.CreateQuotationRequest{
		CustomerLeadID: uuid.NewString(), Items: ordersSampleItems(), Note: "scoped quote",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("tenant A admin create quotation: %d %s", created.status, created.body)
	}
	var cq ordersports.CreateQuotationResponse
	if err := json.Unmarshal(created.body, &cq); err != nil {
		t.Fatalf("decode: %v", err)
	}
	path := "/api/v1/orders/quotations/" + cq.QuotationID
	if r := f.doWithHeaders(t, http.MethodGet, path, op, nil, scopeHdr(a.TenantID)); r.status != http.StatusOK {
		t.Errorf("scoped read in A: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, path, a.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("tenant A's own admin must see the quotation: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, path, op, nil, scopeHdr(b.TenantID)); r.status != http.StatusNotFound {
		t.Errorf("scoped to B must not see A's quotation: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, path, b.AccessToken, nil); r.status != http.StatusNotFound {
		t.Errorf("tenant B must not see A's quotation: %d %s", r.status, r.body)
	}
}
