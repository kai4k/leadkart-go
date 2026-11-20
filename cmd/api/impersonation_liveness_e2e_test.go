//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// ADR 0069 D2 — an impersonation token is only as alive as its session: ending
// the session (or letting it expire) kills the token on EVERY route, not only on
// authority commands.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Impersonation_EndedSessionIsDeadOnEveryRoute(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	p := permission.IdentityPermissions
	opTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
	sess := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", opTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: "liveness e2e session", DurationMinutes: 30})
	if sess.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", sess.status, sess.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(sess.body, &out); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	routes := []struct{ name, method, path string }{
		{"identity authority route", http.MethodGet, "/api/v1/roles"},
		{"another module's route", http.MethodGet, "/api/v1/inventory/products"},
	}
	for _, r := range routes {
		if got := f.authedJSON(t, r.method, r.path, out.AccessToken, nil); got.status != http.StatusOK {
			t.Fatalf("%s while the session is live: %d %s", r.name, got.status, got.body)
		}
	}
	// An authority write: assign a role to a member (POST /users answers 500 for a
	// session today - its synthetic membership id violates created_by's FK; reported, not part of this fix).
	member := f.createMember(t, tenantA, "liveness")
	salesExec := f.roleIDByName(t, tenantA.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
	if got := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+member.membershipID+"/roles", out.AccessToken, ports.AssignUserRoleRequest{RoleID: salesExec}); got.status != http.StatusNoContent {
		t.Fatalf("authority write while live: %d %s", got.status, got.body)
	}

	if end := f.authedJSON(t, http.MethodDelete, "/api/v1/platform/impersonation/sessions/"+out.SessionID, opTok, nil); end.status != http.StatusNoContent {
		t.Fatalf("end session: %d %s", end.status, end.body)
	}
	for _, r := range routes {
		got := f.authedJSON(t, r.method, r.path, out.AccessToken, nil)
		if got.status != http.StatusUnauthorized || errCodeOf(got) != "stale_token" {
			t.Errorf("%s after the session ended: %d %.80s; want 401 stale_token", r.name, got.status, got.body)
		}
	}
	member2 := f.createMember(t, tenantA, "liveness2")
	if got := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+member2.membershipID+"/roles", out.AccessToken, ports.AssignUserRoleRequest{RoleID: salesExec}); got.status != http.StatusUnauthorized {
		t.Errorf("authority write after the session ended: %d %s; want 401", got.status, got.body)
	}
	// The operator's own token is unaffected by ending the session.
	if got := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", opTok, nil); got.status != http.StatusOK {
		t.Errorf("operator token after ending the session: %d %s", got.status, got.body)
	}
}
