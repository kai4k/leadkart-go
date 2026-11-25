//go:build integration

// arch-test:no-timeout-needed — one test over newE2EFixture, which bounds its own startup; per-request
// HTTP uses t.Context().

// G2o B1 (ADR 0073 5a): LeadKart staff act in LeadKart's own tenant directly. A session opened into the
// platform tenant would hold tenant.admin there without any platform permission, so it is refused and
// the managed-revoke lift the G2n review found (an impersonation token lifting identity.roles.manage's
// revoke from a staff member) cannot be reached.

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Impersonation_PlatformTenantTargetIsRefused(t *testing.T) {
	f := newE2EFixture(t)
	platform := f.registerPlatformTenant(t)
	p := permission.IdentityPermissions
	operatorTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
	r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", operatorTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: platform.TenantID, Reason: "ticket 4712 platform tenant session", DurationMinutes: 30})
	if r.status != http.StatusUnprocessableEntity || errCodeOf(r) != "impersonation_invalid" {
		t.Fatalf("session into the platform tenant: %d %s; want 422 impersonation_invalid", r.status, r.body)
	}
	// a company is still open to a session (control)
	company := f.registerAndLogin(t, "impplatctl")
	if _, sess := f.openImpersonation(t, company.TenantID); sess.AccessToken == "" {
		t.Fatal("control: a session into a company must open")
	}
}
