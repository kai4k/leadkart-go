//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// ADR 0039 / ADR 0068 D7: X-Tenant-Id operator scope selection end to end
// against real Postgres (RLS) and the real mux.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	invports "github.com/leadkart/leadkart-go/internal/inventory/ports"
)

const unknownTenantID = "0190a000-0000-7000-8000-0000000000ee"

func scopeHdr(tenantID string) map[string]string { return map[string]string{"X-Tenant-Id": tenantID} }

func userIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var lr ports.ListUsersResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	out := make([]string, 0, len(lr.Users))
	for _, u := range lr.Users {
		out = append(out, u.MembershipID)
	}
	return out
}

func roleTenantIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var lr ports.ListRolesResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode roles: %v", err)
	}
	out := make([]string, 0, len(lr.Roles))
	for _, r := range lr.Roles {
		out = append(out, r.TenantID)
	}
	return out
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if v, ok := m["error"].(string); ok {
		return v
	}
	v, _ := m["code"].(string)
	return v
}

func TestE2E_TenantScope_HeaderForOperatorsOnly(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "scopea")
	b := f.registerAndLogin(t, "scopeb")
	op := f.mintPlatformTokenWith(t, nil, true)

	// RLS proof: the operator's effective tenant reaches set_config('app.tenant_id').
	resp := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr(a.TenantID))
	if got := userIDs(t, resp.body); resp.status != http.StatusOK || !slices.Contains(got, a.MembershipID) || slices.Contains(got, b.MembershipID) {
		t.Fatalf("operator scoped to A must list exactly A's users: %d %s", resp.status, resp.body)
	}
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr(b.TenantID))
	if got := userIDs(t, resp.body); resp.status != http.StatusOK || !slices.Contains(got, b.MembershipID) || slices.Contains(got, a.MembershipID) {
		t.Fatalf("operator scoped to B must list exactly B's users: %d %s", resp.status, resp.body)
	}
	// No header: the operator's own (synthetic platform) tenant — neither A nor B.
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, nil)
	if got := userIDs(t, resp.body); resp.status != http.StatusOK || slices.Contains(got, a.MembershipID) || slices.Contains(got, b.MembershipID) {
		t.Fatalf("operator without header must stay in own tenant: %d %s", resp.status, resp.body)
	}
	// Roles list follows the same effective tenant.
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/roles", op, nil, scopeHdr(b.TenantID))
	rt := roleTenantIDs(t, resp.body)
	if resp.status != http.StatusOK || len(rt) == 0 || slices.ContainsFunc(rt, func(id string) bool { return id != b.TenantID }) {
		t.Fatalf("operator scoped to B must list only B's roles: %d %s", resp.status, resp.body)
	}

	// Validation.
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr(unknownTenantID)); r.status != http.StatusNotFound || decodeErrorCode(t, r.body) != "tenant_scope_not_found" {
		t.Errorf("unknown tenant: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr("not-a-uuid")); r.status != http.StatusBadRequest || decodeErrorCode(t, r.body) != "invalid_tenant_scope" {
		t.Errorf("malformed: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/tenants/"+b.TenantID, op, nil, scopeHdr(a.TenantID)); r.status != http.StatusBadRequest || decodeErrorCode(t, r.body) != "tenant_scope_conflict" {
		t.Errorf("path/header conflict: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/tenants/"+b.TenantID, op, nil, scopeHdr(b.TenantID)); r.status != http.StatusOK {
		t.Errorf("header equal to path: %d %s", r.status, r.body)
	}

	// Tenant users: header is IGNORED, never honoured, never an error.
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/users", b.AccessToken, nil, scopeHdr(a.TenantID))
	if got := userIDs(t, resp.body); resp.status != http.StatusOK || slices.Contains(got, a.MembershipID) || !slices.Contains(got, b.MembershipID) {
		t.Errorf("tenant user header must be ignored: %d %s", resp.status, resp.body)
	}
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/users", b.AccessToken, nil, scopeHdr("not-a-uuid"))
	if resp.status != http.StatusOK {
		t.Errorf("tenant user malformed header must be ignored: %d %s", resp.status, resp.body)
	}
	resp = f.doWithHeaders(t, http.MethodGet, "/api/v1/roles", b.AccessToken, nil, scopeHdr(a.TenantID))
	if rt := roleTenantIDs(t, resp.body); resp.status != http.StatusOK || slices.ContainsFunc(rt, func(id string) bool { return id != b.TenantID }) {
		t.Errorf("tenant user roles must stay in own tenant: %d %s", resp.status, resp.body)
	}

	// Writes land in the scoped tenant.
	created := f.doWithHeaders(t, http.MethodPost, "/api/v1/users", op,
		ports.CreateUserRequest{Email: "scoped-new@e2e.test", Password: "Scoped-User-2026!", FirstName: "Scoped", LastName: "User"}, scopeHdr(a.TenantID))
	if created.status != http.StatusCreated {
		t.Fatalf("operator create in A: %d %s", created.status, created.body)
	}
	var cu ports.CreateUserResponse
	if err := json.Unmarshal(created.body, &cu); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	role := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", op, ports.CreateRoleRequest{Name: "Scoped Role", HierarchyLevel: 50}, scopeHdr(a.TenantID))
	if role.status != http.StatusCreated {
		t.Fatalf("operator create role in A: %d %s", role.status, role.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/roles", a.AccessToken, nil, nil); !strings.Contains(string(r.body), "Scoped Role") {
		t.Errorf("scoped role not visible to tenant A: %s", r.body)
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/roles", b.AccessToken, nil, nil); strings.Contains(string(r.body), "Scoped Role") {
		t.Errorf("role created under scope A leaked into tenant B")
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", a.AccessToken, nil, nil); !slices.Contains(userIDs(t, r.body), cu.MembershipID) {
		t.Errorf("created user not visible to tenant A's own admin")
	}
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", b.AccessToken, nil, nil); slices.Contains(userIDs(t, r.body), cu.MembershipID) {
		t.Errorf("user created under scope A leaked into tenant B")
	}
}

// An operator without the platform.users.view equivalent is denied even with
// the header, and a tenant-plane grant in the operator's own tenant does not
// carry into the scoped tenant.
func TestE2E_TenantScope_AuthorisationEvaluatedInScopedTenant(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "scopeauth")
	p := permission.IdentityPermissions
	noView := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Meta.TenantAdmin, p.Users.View}, false)
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", noView, nil, scopeHdr(a.TenantID)); r.status != http.StatusForbidden {
		t.Errorf("operator without platform.users.view must get 403 under scope: %d %s", r.status, r.body)
	}
	withView := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView}, false)
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", withView, nil, scopeHdr(a.TenantID)); r.status != http.StatusOK {
		t.Errorf("operator with platform.users.view must read scoped tenant: %d %s", r.status, r.body)
	}
	// Capabilities ignores the header: it reports the caller's own claims.
	r := f.doWithHeaders(t, http.MethodGet, "/api/v1/auth/me/capabilities", withView, nil, scopeHdr(a.TenantID))
	var caps ports.CapabilitiesDto
	if err := json.Unmarshal(r.body, &caps); err != nil || r.status != http.StatusOK || caps.TenantID == a.TenantID {
		t.Errorf("capabilities must ignore the header: %d %s", r.status, r.body)
	}
}

// Cross-module proof (inventory): the operator's effective tenant reaches
// another module's port and its RLS-scoped repository for READS. Writes under a
// foreign header are read-only (tenant_scope_read_only), so the product is
// created by tenant A's own admin.
func TestE2E_TenantScope_InventoryActsInScopedTenant(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "scopeinva")
	b := f.registerAndLogin(t, "scopeinvb")
	op := f.mintPlatformTokenWith(t, nil, true)

	created := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", a.AccessToken, invports.CreateProductRequest{
		SKU: "SCOPE-001", Name: "Scoped Tablet", DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099", GSTRateBps: 1200,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("tenant A admin create product: %d %s", created.status, created.body)
	}
	list := func(token string, hdr map[string]string) invports.ListProductsResponse {
		r := f.doWithHeaders(t, http.MethodGet, "/api/v1/inventory/products", token, nil, hdr)
		if r.status != http.StatusOK {
			t.Fatalf("list products: %d %s", r.status, r.body)
		}
		var lr invports.ListProductsResponse
		if err := json.Unmarshal(r.body, &lr); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return lr
	}
	if lr := list(a.AccessToken, nil); len(lr.Items) != 1 || lr.Items[0].TenantID != a.TenantID {
		t.Errorf("product must belong to tenant A: %+v", lr.Items)
	}
	if lr := list(b.AccessToken, nil); len(lr.Items) != 0 {
		t.Errorf("tenant B must not see A's product: %+v", lr.Items)
	}
	if lr := list(op, scopeHdr(a.TenantID)); len(lr.Items) != 1 {
		t.Errorf("operator scoped to A must see A's product: %+v", lr.Items)
	}
	if lr := list(op, scopeHdr(b.TenantID)); len(lr.Items) != 0 {
		t.Errorf("operator scoped to B must not see A's product: %+v", lr.Items)
	}
}
