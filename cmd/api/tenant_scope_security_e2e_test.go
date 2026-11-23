//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// X-Tenant-Id per-route scope policy end to end (ADR 0039 / ADR 0068 D7):
// routes that must reject, ignore, or conflict-check the operator scope header.

package main

import (
	"encoding/json"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// pendingRequestIn files a tenant.admin elevation request as a fresh member of
// tenant owner and returns the member's token, the request id, and the member's
// membership id.
func (f e2eFixture) pendingRequestIn(t *testing.T, owner registeredTenant, prefix string) (memberToken, requestID, membershipID string) {
	t.Helper()
	m := f.createMember(t, owner, prefix)
	f.assignRole(t, owner.AccessToken, m.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	membershipID, memberToken = m.membershipID, f.loginToken(t, m.email, m.password)
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", memberToken, ports.CreatePermissionRequestRequest{
		Permission: permission.IdentityPermissions.Users.View,
		Reason:     "needs admin access for the quarter",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create permission request: %d %s", resp.status, resp.body)
	}
	var cr ports.CreatePermissionRequestResponse
	if err := json.Unmarshal(resp.body, &cr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return memberToken, cr.RequestID, membershipID
}

// A platform-tenant member WITHOUT operator permissions and a proper operator
// both get 403 tenant_scope_not_supported when they try to decide a tenant-B
// user's permission request through X-Tenant-Id; nothing changes.
func TestE2E_TenantScope_PermissionRequestsRejectScope(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "scopepr")
	memberToken, reqID, memberID := f.pendingRequestIn(t, b, "requester")

	plainPlatformMember := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	operator := f.mintPlatformTokenWith(t, nil, true)

	for name, token := range map[string]string{"platform member without permissions": plainPlatformMember, "operator": operator} {
		for _, action := range []struct {
			verb string
			body any
		}{
			{"approve", ports.ApprovePermissionRequestRequest{DecisionReason: "ok"}},
			{"deny", ports.DenyPermissionRequestRequest{DecisionReason: "no"}},
		} {
			r := f.doWithHeaders(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/"+action.verb, token, action.body, scopeHdr(b.TenantID))
			if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "tenant_scope_not_supported" {
				t.Errorf("%s %s under scope: %d %s", name, action.verb, r.status, r.body)
			}
		}
		// Reads under scope are rejected too (the whole route family).
		if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/permission-requests/"+reqID, token, nil, scopeHdr(b.TenantID)); r.status != http.StatusForbidden {
			t.Errorf("%s get under scope: %d %s", name, r.status, r.body)
		}
	}

	// Still pending: the approve flow flips state and writes the grant in one
	// transaction, so a pending request proves no permission was conferred.
	r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+reqID, memberToken, nil)
	var dto ports.PermissionRequestDto
	if err := json.Unmarshal(r.body, &dto); err != nil || r.status != http.StatusOK || dto.State != "pending" || dto.RequesterMembershipID != memberID {
		t.Fatalf("request must stay pending: %d %s", r.status, r.body)
	}
}

// I1: a marketplace purchase debits the caller's own tenant wallet; under an
// operator scope it would debit the operator's tenant while naming B, so the
// route rejects the scope outright.
func TestE2E_TenantScope_LeadPurchaseRejectsScope(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "scopebuy")
	op := f.mintPlatformTokenWith(t, []string{permission.IdentityPermissions.PlatformMarketplace.Purchase}, true)
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/platform/marketplace/leads/"+unknownTenantID+"/purchase", op, struct{}{}, scopeHdr(b.TenantID))
	if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "tenant_scope_not_supported" {
		t.Fatalf("purchase under scope: %d %s", r.status, r.body)
	}
}

// I2: auth/* and platform/* routes ignore the header entirely — an operator
// sending an UNKNOWN or malformed tenant id still gets a normal answer, and
// POST /tenants (a new tenant has no target tenant) does too.
func TestE2E_TenantScope_IgnoredOnAuthAndPlatformRoutes(t *testing.T) {
	f := newE2EFixture(t)
	op := f.mintPlatformTokenWith(t, nil, true)
	for _, hdr := range []string{unknownTenantID, "not-a-uuid"} {
		for _, path := range []string{"/api/v1/auth/me/capabilities", "/api/v1/auth/sessions", "/api/v1/auth/me/activity", "/api/v1/platform/stats", "/api/v1/platform/tenants"} {
			if r := f.doWithHeaders(t, http.MethodGet, path, op, nil, scopeHdr(hdr)); r.status != http.StatusOK {
				t.Errorf("GET %s with header %q: %d %s", path, hdr, r.status, r.body)
			}
		}
	}
	// The same header IS validated on a tenant-plane route (control).
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr(unknownTenantID)); r.status != http.StatusNotFound {
		t.Errorf("tenant-plane control: %d %s", r.status, r.body)
	}
	// POST /tenants with an unknown header: the registration runs (a bad body
	// is the handler's 4xx, never tenant_scope_not_found).
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants", op, ports.RegisterTenantRequest{}, scopeHdr(unknownTenantID))
	if code := decodeErrorCode(t, r.body); code == "tenant_scope_not_found" || code == "invalid_tenant_scope" {
		t.Errorf("POST /tenants must ignore the header: %d %s", r.status, r.body)
	}
}

// I3 + M4: lifecycle / hard-delete routes conflict-check the header against
// the path tenant; scoped writes into a suspended tenant are 409 while reads
// and the lifecycle routes still work.
func TestE2E_TenantScope_LifecycleConflictAndSuspendedTenant(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "lifea")
	b := f.registerAndLogin(t, "lifeb")
	op := f.mintPlatformTokenWith(t, []string{
		permission.IdentityPermissions.Platform.TenantsManage,
	}, true)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/tenants/" + b.TenantID + "/suspend"},
		{http.MethodPost, "/api/v1/tenants/" + b.TenantID + "/activate"},
	} {
		r := f.doWithHeaders(t, route.method, route.path, op, struct{}{}, scopeHdr(a.TenantID))
		if r.status != http.StatusBadRequest || decodeErrorCode(t, r.body) != "tenant_scope_conflict" {
			t.Errorf("%s %s with conflicting header: %d %s", route.method, route.path, r.status, r.body)
		}
	}

	// Suspend B (header equal to the path tenant is fine).
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants/"+b.TenantID+"/suspend", op, ports.SuspendTenantRequest{Reason: "e2e suspension"}, scopeHdr(b.TenantID)); r.status >= 300 {
		t.Fatalf("suspend B: %d %s", r.status, r.body)
	}
	// Scoped read into the suspended tenant works; scoped write is 409.
	if r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", op, nil, scopeHdr(b.TenantID)); r.status != http.StatusOK {
		t.Errorf("scoped read into suspended tenant: %d %s", r.status, r.body)
	}
	w := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", op, ports.CreateRoleRequest{Name: "Nope", HierarchyLevel: 50}, scopeHdr(b.TenantID))
	if w.status != http.StatusConflict || decodeErrorCode(t, w.body) != "tenant_not_active" {
		t.Errorf("scoped write into suspended tenant: %d %s", w.status, w.body)
	}
	// Lifecycle route still works under scope, and writes resume afterwards.
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants/"+b.TenantID+"/activate", op, struct{}{}, scopeHdr(b.TenantID)); r.status >= 300 {
		t.Fatalf("activate B under scope: %d %s", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", op, ports.CreateRoleRequest{Name: "Fine", HierarchyLevel: 50}, scopeHdr(b.TenantID)); r.status != http.StatusCreated {
		t.Errorf("scoped write after activation: %d %s", r.status, r.body)
	}
}

// I4: a scoped operator write leaves a durable trail: the outbox row carries
// act_operator_id = the operator's person id, act_reason, and tenant_id = the
// scoped tenant. A tenant user's own write (header ignored) carries none.
func TestE2E_TenantScope_ScopedWriteCarriesOperatorAttribution(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "scopeattr")
	operatorID := ids.NewV7().String()
	op := mintOperatorJWT(t, f.Issuer, f.persons, operatorID, nil, true)

	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", op, ports.CreateRoleRequest{Name: "Attributed Role", HierarchyLevel: 50}, scopeHdr(b.TenantID))
	if r.status != http.StatusCreated {
		t.Fatalf("scoped create role: %d %s", r.status, r.body)
	}
	var scoped []messagingtest.OutboxRow
	for _, row := range messagingtest.DrainOutbox(t.Context(), t, f.Pool) {
		if row.Message.Metadata.Get(messaging.HeaderTenantID) == b.TenantID && row.Message.Metadata.Get(messaging.HeaderActOperatorID) != "" {
			scoped = append(scoped, row)
		}
	}
	if len(scoped) == 0 {
		t.Fatal("scoped write must leave an attributed outbox row")
	}
	for _, row := range scoped {
		if got := row.Message.Metadata.Get(messaging.HeaderActOperatorID); got != operatorID {
			t.Errorf("act_operator_id = %s, want %s", got, operatorID)
		}
		if got := row.Message.Metadata.Get(messaging.HeaderActReason); got != "tenant-via-header" {
			t.Errorf("act_reason = %s", got)
		}
	}
	scopedCount := len(scoped)

	// Control: the tenant admin's own role creation (header ignored) adds no
	// act_operator_id row.
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", b.AccessToken, ports.CreateRoleRequest{Name: "Own Role", HierarchyLevel: 40}, scopeHdr(unknownTenantID)); r.status != http.StatusCreated {
		t.Fatalf("own create role: %d %s", r.status, r.body)
	}
	attributed := 0
	for _, row := range messagingtest.DrainOutbox(t.Context(), t, f.Pool) {
		if row.Message.Metadata.Get(messaging.HeaderTenantID) == b.TenantID && row.Message.Metadata.Get(messaging.HeaderActOperatorID) != "" {
			attributed++
		}
	}
	if attributed != scopedCount {
		t.Errorf("a tenant user's own write must not carry act_operator_id (%d attributed rows, want %d)", attributed, scopedCount)
	}
}
