//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// X-Tenant-Id per-route scope policy end to end (ADR 0039 / ADR 0068 D7):
// routes that must reject, ignore, or conflict-check the operator scope header.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// pendingRequestIn files a tenant.admin elevation request as a fresh member of
// tenant owner and returns the member's token, the request id, and the member's
// membership id.
func (f e2eFixture) pendingRequestIn(t *testing.T, owner registeredTenant, prefix string) (memberToken, requestID, membershipID string) {
	t.Helper()
	membershipID, memberToken = f.createMemberAndLogin(t, owner, prefix)
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", memberToken, ports.CreatePermissionRequestRequest{
		Permission: permission.IdentityPermissions.Meta.TenantAdmin,
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
