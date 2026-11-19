//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request HTTP uses t.Context().

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// TestE2E_RegisterTenant_RequiresPlatformOperator — tenant onboarding is
// operator-only (ADR 0053 "no public sign-up surface"; ADR 0068 D6).
func TestE2E_RegisterTenant_RequiresPlatformOperator(t *testing.T) {
	f := newE2EFixture(t)
	body := ports.RegisterTenantRequest{Slug: "gate-co", LegalName: "Gate Co Pvt Ltd", DisplayName: "Gate",
		AdminEmail: "gate-admin@e2e.test", AdminPassword: "Gate-Admin-2026!x", AdminFirstName: "Gate", AdminLastName: "Admin"}
	if resp := f.postJSON(t, "/api/v1/tenants", body); resp.status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", resp.status)
	}
	member := f.registerAndLogin(t, "notop")
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/tenants", member.AccessToken, body); resp.status != http.StatusForbidden {
		t.Fatalf("tenant admin: %d, want 403", resp.status)
	}
	op := f.mintPlatformTokenWith(t, nil, true)
	if resp := f.authedJSON(t, http.MethodPost, "/api/v1/tenants", op, body); resp.status != http.StatusCreated {
		t.Fatalf("platform super user: %d %s, want 201", resp.status, resp.body)
	}
}
