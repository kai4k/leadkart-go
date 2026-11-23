//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// Plan D9 (ADR 0045 "refuse impersonation tokens at endpoints that shouldn't
// accept them") — what an impersonation session can and cannot reach, and who is
// attributed for what it writes.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	invports "github.com/leadkart/leadkart-go/internal/inventory/ports"
	tasksports "github.com/leadkart/leadkart-go/internal/tasks/ports"
)

// openImpersonation opens a session for the operator against tenant and returns
// the operator's token, the session's scoped token and the session id.
func (f e2eFixture) openImpersonation(t *testing.T, tenantID string) (operatorTok string, out ports.CreateImpersonationSessionResponse) {
	t.Helper()
	p := permission.IdentityPermissions
	operatorTok = mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", operatorTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantID, Reason: "ticket 4711 reach e2e session", DurationMinutes: 30})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return operatorTok, out
}

func (f e2eFixture) requireImpersonationRefused(t *testing.T, what, method, path, token string, body any) {
	t.Helper()
	r := f.authedJSON(t, method, path, token, body)
	if r.status != http.StatusForbidden || errCodeOf(r) != "impersonation_not_allowed" {
		t.Errorf("%s: %s %s answered %d %s; want 403 impersonation_not_allowed", what, method, path, r.status, r.body)
	}
}

// An impersonation session is refused on every platform-plane route, on the
// self-service routes that act on "the caller", on tenant registration, the
// lifecycle commands and omni-search, and NOTHING changes; it is
// still allowed on ordinary tenant routes, and dead once the session ends.
func TestE2E_Impersonation_RefusalList_RefusedWithStateUnchangedAndOrdinaryRoutesOpen(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "impreach")
	operatorTok, sess := f.openImpersonation(t, tenantA.TenantID)
	tok := sess.AccessToken
	const someID = "0190a000-0000-7000-8000-0000000000cc"

	// Ordinary tenant routes stay open: a read, a create in two modules.
	for _, path := range []string{"/api/v1/users", "/api/v1/roles", "/api/v1/inventory/products", "/api/v1/auth/me/capabilities", "/api/v1/permission-requests"} {
		if r := f.authedJSON(t, http.MethodGet, path, tok, nil); r.status != http.StatusOK {
			t.Errorf("GET %s under a session: %d %s; want 200", path, r.status, r.body)
		}
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users", tok, ports.CreateUserRequest{
		Email: "created-in-reach@e2e.test", Password: "Sup3r-secret-pass!", FirstName: "Reach", LastName: "Created",
	}); r.status != http.StatusCreated {
		t.Fatalf("create a user under a session: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/inventory/products", tok, invports.CreateProductRequest{
		SKU: "IMP-001", Name: "Imp Tablet", DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099", GSTRateID: e2eRate(t, f, tenantA.AccessToken, 1200),
	}); r.status != http.StatusCreated {
		t.Fatalf("create a product under a session: %d %s", r.status, r.body)
	}

	// Self-service routes act on the caller: refused.
	f.requireImpersonationRefused(t, "change password", http.MethodPost, "/api/v1/auth/change-password", tok,
		map[string]string{"current_password": tenantA.Password, "new_password": "Hijacked-Pass-2026!x"})
	f.requireImpersonationRefused(t, "change email", http.MethodPost, "/api/v1/auth/request-email-change", tok, map[string]string{"new_email": "hijack@e2e.test"})
	f.requireImpersonationRefused(t, "list sessions", http.MethodGet, "/api/v1/auth/sessions", tok, nil)
	f.requireImpersonationRefused(t, "revoke a session", http.MethodDelete, "/api/v1/auth/sessions/"+someID, tok, nil)
	f.requireImpersonationRefused(t, "revoke all sessions", http.MethodDelete, "/api/v1/auth/sessions", tok, nil)
	f.requireImpersonationRefused(t, "own activity", http.MethodGet, "/api/v1/auth/me/activity", tok, nil)
	f.requireImpersonationRefused(t, "submit a permission request", http.MethodPost, "/api/v1/permission-requests", tok,
		ports.CreatePermissionRequestRequest{Permission: "crm.leads.read", DurationDays: 1, Reason: "reach e2e"})
	f.requireImpersonationRefused(t, "approve a request", http.MethodPost, "/api/v1/permission-requests/"+someID+"/approve", tok, map[string]string{})
	f.requireImpersonationRefused(t, "deny a request", http.MethodPost, "/api/v1/permission-requests/"+someID+"/deny", tok,
		ports.DenyPermissionRequestRequest{DecisionReason: "reach e2e deny"})
	f.requireImpersonationRefused(t, "cancel a request", http.MethodPost, "/api/v1/permission-requests/"+someID+"/cancel", tok, map[string]string{})

	// The platform plane: no sub-impersonation, no listing or ending sessions,
	// no stats, no person administration, no marketplace spending, no top-ups.
	f.requireImpersonationRefused(t, "open another session", http.MethodPost, "/api/v1/platform/impersonation/sessions", tok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: "sub-impersonation attempt", DurationMinutes: 30})
	f.requireImpersonationRefused(t, "list sessions", http.MethodGet, "/api/v1/platform/impersonation/sessions", tok, nil)
	f.requireImpersonationRefused(t, "end the session", http.MethodDelete, "/api/v1/platform/impersonation/sessions/"+sess.SessionID, tok, nil)
	f.requireImpersonationRefused(t, "platform stats", http.MethodGet, "/api/v1/platform/stats", tok, nil)
	f.requireImpersonationRefused(t, "platform tenants", http.MethodGet, "/api/v1/platform/tenants", tok, nil)
	f.requireImpersonationRefused(t, "platform persons", http.MethodGet, "/api/v1/platform/persons", tok, nil)
	f.requireImpersonationRefused(t, "marketplace purchase", http.MethodPost, "/api/v1/platform/marketplace/leads/"+someID+"/purchase", tok, map[string]string{})
	f.requireImpersonationRefused(t, "lead credit top-up", http.MethodPost, "/api/v1/platform/lead-credits/topup", tok, map[string]string{})
	// The two tenant-plane reads under /platform/ are NOT refused: they read the
	// supported tenant's own wallet and catalogue under tenant permissions (ADR 0069).
	// Observed against Postgres: both answer 200. The wallet is the SUPPORTED
	// tenant's (the token's tenant), the catalogue is the unfiltered marketplace.
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/lead-credits/balance", tok, nil); r.status != http.StatusOK {
		t.Errorf("GET lead-credits/balance under a session = %d %s; want 200", r.status, r.body)
	} else if want := `{"tenant_id":"` + tenantA.TenantID + `","balance":0}`; strings.TrimSpace(string(r.body)) != want {
		t.Errorf("GET lead-credits/balance under a session = %s; want %s", r.body, want)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/marketplace/leads", tok, nil); r.status != http.StatusOK {
		t.Errorf("GET marketplace/leads under a session = %d %s; want 200", r.status, r.body)
	} else if want := `{"items":[],"has_more":false}`; strings.TrimSpace(string(r.body)) != want {
		t.Errorf("GET marketplace/leads under a session = %s; want %s", r.body, want)
	}

	// Tenant-path routes that are platform-anchored.
	f.requireImpersonationRefused(t, "register a tenant", http.MethodPost, "/api/v1/tenants", tok, map[string]string{})
	f.requireImpersonationRefused(t, "suspend the tenant", http.MethodPost, "/api/v1/tenants/"+tenantA.TenantID+"/suspend", tok, ports.SuspendTenantRequest{Reason: "reach e2e suspend"})
	f.requireImpersonationRefused(t, "omni-search", http.MethodGet, "/api/v1/search?q=impreach", tok, nil)

	// State is unchanged: the password still logs in, the tenant is not suspended
	// or deleted, the owner's refresh session is intact, the session still lives.
	if lr := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: tenantA.Email, Password: tenantA.Password, DeviceLabel: "e2e"}); lr.status != http.StatusOK {
		t.Errorf("the old password must still log in: %d %s", lr.status, lr.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+tenantA.TenantID, tenantA.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("the tenant must be untouched: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tenantA.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("the owner's token and sessions must be untouched: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", tok, nil); r.status != http.StatusOK {
		t.Errorf("the session must still be alive after the refused end attempt: %d %s", r.status, r.body)
	}
	list := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", operatorTok, nil)
	if list.status != http.StatusOK {
		t.Errorf("operator lists sessions: %d %s", list.status, list.body)
	}

	// Dead after the session ends: the operator ends it with their own token.
	if end := f.authedJSON(t, http.MethodDelete, "/api/v1/platform/impersonation/sessions/"+sess.SessionID, operatorTok, nil); end.status != http.StatusNoContent {
		t.Fatalf("end session: %d %s", end.status, end.body)
	}
	for _, path := range []string{"/api/v1/users", "/api/v1/inventory/products", "/api/v1/auth/me/capabilities"} {
		if r := f.authedJSON(t, http.MethodGet, path, tok, nil); r.status != http.StatusUnauthorized || errCodeOf(r) != "stale_token" {
			t.Errorf("GET %s after the session ended: %d %s; want 401 stale_token", path, r.status, r.body)
		}
	}
	// A refused route answers 401 (dead session), not the refusal, once the session is over.
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tok, nil); r.status != http.StatusUnauthorized {
		t.Errorf("a refused route after the session ended: %d %s; want 401", r.status, r.body)
	}
}

// An impersonation session is Admin-equivalent, never owner-level: it cannot make
// anyone a Company Owner, nor take or confer tenant.admin (the authority policy,
// verified through the HTTP surface).
func TestE2E_Impersonation_CannotChangeTheTenantsOwnership(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "impown")
	member := f.createMember(t, tenantA, "impown-m")
	ownerRole := f.roleIDByName(t, tenantA.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	_, sess := f.openImpersonation(t, tenantA.TenantID)

	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+member.membershipID+"/roles", sess.AccessToken, ports.AssignUserRoleRequest{RoleID: ownerRole})
	if r.status != http.StatusForbidden {
		t.Fatalf("assign Company Owner under a session: %d %s; want 403", r.status, r.body)
	}
	r = f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+member.membershipID+"/permission-overrides", sess.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{permission.IdentityPermissions.Meta.TenantAdmin}, Revoked: []string{}})
	if r.status != http.StatusForbidden {
		t.Fatalf("grant tenant.admin under a session: %d %s; want 403", r.status, r.body)
	}
	// The owner is untouched.
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users/"+tenantA.MembershipID, tenantA.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("owner read: %d %s", r.status, r.body)
	}
}

// Every write a session makes is attributed to the operator behind it, in every
// module: the act claim travels from the token to the outbox rows (act_operator_id
// and the session's reason). Spot-checked across identity, inventory and tasks.
func TestE2E_Impersonation_WritesInEveryModuleCarryTheOperator(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "impattr")
	_, sess := f.openImpersonation(t, tenantA.TenantID)
	claims, err := f.Issuer.Verify(sess.AccessToken)
	if err != nil || claims.Act == nil {
		t.Fatalf("verify session token: %v", err)
	}
	operatorID := claims.Act.Sub

	// Attribution is judged on the rows THIS session writes: everything in the
	// outbox before the writes (registration) is somebody else's.
	before := len(messagingtest.DrainOutbox(t.Context(), t, f.Pool))
	writes := []struct {
		module, method, path string
		body                 any
		want                 int
	}{
		{"identity", http.MethodPost, "/api/v1/users", ports.CreateUserRequest{
			Email: "attributed@e2e.test", Password: "Sup3r-secret-pass!", FirstName: "Attr", LastName: "Member"}, http.StatusCreated},
		{"inventory", http.MethodPost, "/api/v1/inventory/products", invports.CreateProductRequest{
			SKU: "ATTR-001", Name: "Attributed Tablet", DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099", GSTRateID: e2eRate(t, f, tenantA.AccessToken, 1200)}, http.StatusCreated},
		{"tasks", http.MethodPost, "/api/v1/tasks/work-items", tasksports.CreateWorkItemRequest{
			Type: "follow_up", Title: "Attributed follow-up", AssignedToMembershipID: tenantA.MembershipID, DueAt: time.Now().Add(48 * time.Hour)}, http.StatusCreated},
	}
	for _, w := range writes {
		before := len(messagingtest.DrainOutbox(t.Context(), t, f.Pool))
		if r := f.authedJSON(t, w.method, w.path, sess.AccessToken, w.body); r.status != w.want {
			t.Fatalf("%s write %s %s: %d %s; want %d", w.module, w.method, w.path, r.status, r.body, w.want)
		}
		rows := messagingtest.DrainOutbox(t.Context(), t, f.Pool)[before:]
		if len(rows) == 0 {
			t.Errorf("%s write left no outbox row", w.module)
		}
		for _, row := range rows {
			if got := row.Message.Metadata.Get(messaging.HeaderActOperatorID); got != operatorID {
				t.Errorf("%s write: outbox row %s has act_operator_id=%q, want the operator %q",
					w.module, row.Message.Metadata.Get(messaging.HeaderEventType), got, operatorID)
			}
			if row.Message.Metadata.Get(messaging.HeaderActReason) == "" {
				t.Errorf("%s write: outbox row %s has no act_reason", w.module, row.Message.Metadata.Get(messaging.HeaderEventType))
			}
		}
	}
	if len(messagingtest.DrainOutbox(t.Context(), t, f.Pool)) <= before {
		t.Fatal("the session wrote nothing")
	}
}
