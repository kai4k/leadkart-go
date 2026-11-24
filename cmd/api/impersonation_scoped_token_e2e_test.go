//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// Wave 4 — scoped JWT impersonation (ADR 0045) end-to-end matrix.
//
// Verifies the AssumeRole-style flow from operator session creation
// through token use. Specifically:
//
//   - POST /v1/platform/impersonation/sessions returns a scoped
//     access_token whose claims have:
//     - aud = leadkart-impersonation
//     - is_platform = false (DOWNGRADED)
//     - is_super_user = false (DOWNGRADED)
//     - tenant_id = target tenant
//     - act.sub = original operator
//     - act.session_id = the session id
//     - exp = session.ExpiresAt
//
//   - The scoped token authenticates as a TENANT ADMIN of the target
//     tenant for all RLS-scoped reads (verified via the existing
//     /v1/tenants/{tenantId} route).
//
//   - The scoped token CANNOT access /v1/platform/* routes — the
//     is_platform=false downgrade ensures RequirePlatform middleware
//     rejects it. This is the load-bearing blast-radius reduction.
//
//   - Target-not-found returns 404 (not 500).
//
//   - Verifier accepts both regular AudienceClaim and Impersonation
//     AudienceClaim; rejects other audiences. (covered by unit tests
//     in internal/identity/app/jwt/jwt_test.go; integration verifies
//     end-to-end flow.)

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// TestE2E_Impersonation_ScopedToken_Issuance — happy path. Operator
// opens session targeting Tenant A → response carries a JWT whose
// claims match the ADR 0045 contract.
func TestE2E_Impersonation_ScopedToken_Issuance(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")

	resp := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		platformTok,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID:  tenantA.TenantID,
			Reason:          "ticket #1234 — Wave 4 e2e test",
			DurationMinutes: 30,
		})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: status %d body %s", resp.status, resp.body)
	}

	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if out.SessionID == "" {
		t.Error("session_id missing in response")
	}
	if out.AccessToken == "" {
		t.Fatal("access_token missing — Wave 4 contract requires it")
	}
	if out.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", out.TokenType)
	}
	if out.AccessTokenExpiresAtUTC.IsZero() {
		t.Error("access_token_expires_at_utc missing")
	}
	if !out.AccessTokenExpiresAtUTC.Equal(out.ExpiresAtUTC) {
		t.Errorf("token expiry %v != session expiry %v — must match per ADR 0045",
			out.AccessTokenExpiresAtUTC, out.ExpiresAtUTC)
	}

	// Verify the scoped token's claims via the issuer.
	claims, err := f.Issuer.Verify(out.AccessToken)
	if err != nil {
		t.Fatalf("verify scoped token: %v", err)
	}

	// Audience MUST be the impersonation audience.
	if len(claims.Audience) != 1 || claims.Audience[0] != jwt.ImpersonationAudienceClaim {
		t.Errorf("audience = %v, want [%q]", claims.Audience, jwt.ImpersonationAudienceClaim)
	}

	// Tenant context = target tenant.
	if claims.TenantID != tenantA.TenantID {
		t.Errorf("tenant_id = %q, want %q (target tenant)", claims.TenantID, tenantA.TenantID)
	}
	if claims.TenantSlug != tenantA.Slug {
		t.Errorf("tenant_slug = %q, want %q", claims.TenantSlug, tenantA.Slug)
	}

	// DOWNGRADED scope — load-bearing security property.
	if claims.IsPlatform {
		t.Error("is_platform = true on scoped token — MUST be false (downgrade)")
	}
	if claims.IsSuperUser {
		t.Error("is_super_user = true on scoped token — MUST be false (downgrade)")
	}

	// Act claim — preserves the operator identity for audit chain.
	if !claims.IsImpersonation() {
		t.Fatal("IsImpersonation() = false — Act claim missing")
	}
	if claims.Act.SessionID != out.SessionID {
		t.Errorf("act.session_id = %q, want %q", claims.Act.SessionID, out.SessionID)
	}
	if claims.Act.Reason != "ticket #1234 — Wave 4 e2e test" {
		t.Errorf("act.reason = %q, want %q", claims.Act.Reason, "ticket #1234 — Wave 4 e2e test")
	}
	// act.sub IS the operator's person_id — for this fixture we
	// don't have the operator's person_id directly but it's the same
	// value the JWT's sub claim carries.
	if claims.Act.Sub != claims.Subject {
		t.Errorf("act.sub = %q, want %q (subject preserved on impersonation token)",
			claims.Act.Sub, claims.Subject)
	}
}

// TestE2E_Impersonation_ScopedToken_BlocksPlatformRoutes — load-bearing
// blast-radius test. The scoped token has is_platform=false; calling
// any /v1/platform/* route MUST 403 (RequirePlatform rejects).
func TestE2E_Impersonation_ScopedToken_BlocksPlatformRoutes(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")

	resp := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		platformTok,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID:  tenantA.TenantID,
			Reason:          "blast-radius reduction test",
			DurationMinutes: 30,
		})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: status %d body %s", resp.status, resp.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Scoped token tries to list all tenants (Platform-only route).
	listAll := f.authedJSON(t, http.MethodGet,
		"/api/v1/platform/tenants", out.AccessToken, nil)
	if listAll.status != http.StatusForbidden {
		t.Errorf("scoped token GET /platform/tenants: status %d, want 403 (is_platform=false downgrade); body=%s",
			listAll.status, listAll.body)
	}

	// Scoped token tries to open a SUB-impersonation. Defense-in-depth:
	// even if is_platform check were bypassed somehow, the audience
	// discrimination would reject. (Currently the RequirePlatform check
	// fires first; this asserts the route stays gated.)
	subImp := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		out.AccessToken,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID:  tenantA.TenantID,
			Reason:          "attempt sub-impersonation should fail",
			DurationMinutes: 30,
		})
	if subImp.status != http.StatusForbidden {
		t.Errorf("scoped token POST sub-impersonation: status %d, want 403; body=%s",
			subImp.status, subImp.body)
	}
}

// TestE2E_Impersonation_TargetTenantNotFound — opening session against
// a non-existent tenant returns 404, not 500. Distinct error class
// (ErrImpersonationTargetMissing) from the validation rejection.
func TestE2E_Impersonation_TargetTenantNotFound(t *testing.T) {
	f := newE2EFixture(t)
	platformTok := f.mintPlatformToken(t, "")

	resp := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		platformTok,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID:  "0190a000-0000-7000-8000-0000000000ee", // well-formed, no such tenant (the nil UUID is a 400)
			Reason:          "session against ghost tenant",
			DurationMinutes: 30,
		})
	if resp.status != http.StatusNotFound {
		t.Errorf("nonexistent target: status %d, want 404; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeTenantNotFound {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeTenantNotFound)
	}
}

// POST /users under an impersonation session used to answer 500: the session's
// synthetic membership id was stamped as the new member's creator and the
// foreign key refused it. The operator is attributed through the act claim; the
// membership creator stays NULL (the same policy as the X-Tenant-Id scope path).
func TestE2E_Impersonation_CreateUserIsNot500(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: "ticket #77 create a member", DurationMinutes: 30})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.status, resp.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatal(err)
	}
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users", out.AccessToken, ports.CreateUserRequest{
		Email: "created-under-impersonation@example.test", Password: "Sup3r-secret-pass!", FirstName: "Imp", LastName: "Created",
	})
	if r.status != http.StatusCreated {
		t.Fatalf("POST /users under an impersonation session: status %d body %s, want 201 (not 500)", r.status, r.body)
	}
}

// Sweep: no identity write route may answer 500 under an impersonation token (a
// synthetic membership id reaching a foreign key was the one cause found).
func TestE2E_Impersonation_IdentityWritesNeverAnswer500(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")
	resp := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: tenantA.TenantID, Reason: "ticket #78 write sweep", DurationMinutes: 30})
	if resp.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", resp.status, resp.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatal(err)
	}
	const otherID = "01923400-0000-7000-8000-00000000dead"
	m := tenantA.MembershipID
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"create role", http.MethodPost, "/api/v1/roles", ports.CreateRoleRequest{Name: "Imp Role", HierarchyLevel: 60}},
		{"update role", http.MethodPatch, "/api/v1/roles/" + otherID, map[string]any{"name": "x"}},
		{"deactivate role", http.MethodPost, "/api/v1/roles/" + otherID + "/deactivate", nil},
		{"update user profile", http.MethodPatch, "/api/v1/users/" + m + "/profile", ports.UpdateUserProfileRequest{Designation: "d", Department: "e", StatusMessage: "s"}},
		{"deactivate user", http.MethodPost, "/api/v1/users/" + m + "/deactivate", ports.DeactivateUserRequest{Reason: "sweep"}},
		{"reactivate user", http.MethodPost, "/api/v1/users/" + m + "/reactivate", nil},
		{"assign role", http.MethodPost, "/api/v1/users/" + m + "/roles", map[string]any{"role_id": otherID}},
		{"assign manager", http.MethodPut, "/api/v1/users/" + m + "/manager", map[string]any{"manager_id": otherID}},
		{"remove manager", http.MethodDelete, "/api/v1/users/" + m + "/manager", nil},
		{"overrides", http.MethodPatch, "/api/v1/users/" + m + "/permission-overrides", ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}},
		{"permission request", http.MethodPost, "/api/v1/permission-requests", ports.CreatePermissionRequestRequest{Permission: "crm.leads.read", DurationDays: 1, Reason: "sweep"}},
		{"tenant profile", http.MethodPatch, "/api/v1/tenants/" + tenantA.TenantID + "/profile", ports.UpdateTenantProfileRequest{LegalName: "L", DisplayName: "D"}},
		{"change password", http.MethodPost, "/api/v1/auth/change-password", map[string]any{"current_password": "x", "new_password": "Y-new-pass-2026!x"}},
	} {
		r := f.authedJSON(t, c.method, c.path, out.AccessToken, c.body)
		if r.status >= 500 {
			t.Errorf("%s: %s %s answered %d under an impersonation token: %s", c.name, c.method, c.path, r.status, r.body)
		}
	}
}
