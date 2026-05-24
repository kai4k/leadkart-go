//go:build integration

// Cross-tenant + platform-gate end-to-end tests for the post-A.3 HTTP
// surface. Wires the FULL Identity Application against testcontainers
// Postgres + the real ServeMux via httptest, then exercises the
// security-critical paths that pure-unit tests cannot prove:
//
//   - Tenant A's JWT cannot read/write Tenant B's data
//     (RLS-enforced at the DB layer; tested through the actual
//     http handlers, not the repository).
//   - Tenant Admin cannot reach Platform-tier routes (403).
//   - Platform operator JWT bypasses the same-tenant gate.
//   - Single-Active-Membership invariant blocks email re-use.
//   - Impersonation session lifecycle + reason-too-short rejection.
//   - Stats endpoint reflects real DB state.
//
// Pattern follows http_flow_integration_test's testcontainers fixture
// + httptest.Server. One Postgres container per test (testcontainers-
// go default — fast enough at ~3s per fixture; isolation > shared-
// container speedup at this volume).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// ----- Fixture --------------------------------------------------------------

// e2eFixture wires the full HTTP surface against a fresh testcontainers
// Postgres. Returned helpers:
//   - URL: httptest.Server URL.
//   - Issuer: minting platform-operator JWTs directly (no PlatformOnboarding
//     endpoint exists — operators are seeded operationally).
//   - Pool: for DB-level assertions when needed.
type e2eFixture struct {
	URL     string
	Issuer  *jwt.Issuer
	Pool    *pgxpool.Pool
	app     app.Application
	persons *adapters.PersonRepository
}

func newE2EFixture(t *testing.T) e2eFixture {
	t.Helper()
	pool := startWiredPostgresForHTTP(t)
	cfg := config.AppConfig{
		JWT: config.JWTConfig{
			KeyID:      "test-k1",
			SigningKey: "0123456789abcdef0123456789abcdef",
		},
		Refresh: config.RefreshConfig{
			AbsoluteTTL: 14 * 24 * time.Hour,
		},
	}
	now := func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) }
	hybrid := newTestHybridCache(t)
	wiring, err := buildIdentityApp(pool, hybrid, cfg, now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}
	srv := httptest.NewServer(newServer(silentLogger(), wiring.App, platformapp.Application{}, buildInventoryApp(pool), wiring.Issuer, wiring.StampValidator))
	t.Cleanup(srv.Close)
	return e2eFixture{
		URL:     srv.URL,
		Issuer:  wiring.Issuer,
		Pool:    pool,
		app:     wiring.App,
		persons: wiring.Persons,
	}
}

// registeredTenant captures the IDs + access token of a freshly-
// registered tenant + admin login. Convenience for cross-tenant
// scenarios that always need both registration + an authenticated
// caller.
type registeredTenant struct {
	TenantID     string
	PersonID     string
	MembershipID string
	Slug         string
	Email        string
	Password     string
	AccessToken  string
	RefreshToken string
}

// registerAndLogin registers a fresh tenant + immediately logs in as
// its admin. Slug + email are derived from a UUIDv7 suffix to ensure
// per-test uniqueness when running in parallel.
func (f e2eFixture) registerAndLogin(t *testing.T, namePrefix string) registeredTenant {
	t.Helper()
	suffix := ids.NewV7().String()[len(ids.NewV7().String())-8:]
	slug := namePrefix + "-" + suffix
	email := namePrefix + "-admin-" + suffix + "@e2e.test"
	password := "correct horse battery staple " + suffix

	regResp := f.postJSON(t, "/api/v1/tenants", ports.RegisterTenantRequest{
		Slug:           slug,
		LegalName:      namePrefix + " Pharma Pvt Ltd",
		DisplayName:    namePrefix,
		AdminEmail:     email,
		AdminPassword:  password,
		AdminFirstName: namePrefix,
		AdminLastName:  "Admin",
	})
	if regResp.status != http.StatusCreated {
		t.Fatalf("register %s: status %d body %s", namePrefix, regResp.status, regResp.body)
	}
	var reg ports.RegisterTenantResponse
	if err := json.Unmarshal(regResp.body, &reg); err != nil {
		t.Fatalf("register %s decode: %v", namePrefix, err)
	}
	loginResp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{
		Email:       email,
		Password:    password,
		DeviceLabel: "e2e test device " + suffix,
	})
	if loginResp.status != http.StatusOK {
		t.Fatalf("login %s: status %d body %s", namePrefix, loginResp.status, loginResp.body)
	}
	var login ports.LoginResponse
	if err := json.Unmarshal(loginResp.body, &login); err != nil {
		t.Fatalf("login %s decode: %v", namePrefix, err)
	}
	return registeredTenant{
		TenantID:     reg.TenantID,
		PersonID:     reg.PersonID,
		MembershipID: reg.MembershipID,
		Slug:         slug,
		Email:        email,
		Password:     password,
		AccessToken:  login.AccessToken,
		RefreshToken: login.RefreshToken,
	}
}

// mintPlatformToken issues a synthetic Platform-tier JWT for tests
// that need operator-level access. Operators are seeded operationally
// in production (no HTTP endpoint provisions them — see A.7
// "Deferred"); for testing we mint directly via the issuer.
//
// The operator Person is inserted into the persons table with a
// freshly-generated SecurityStamp; the JWT's `security_stamp` claim
// is bound to that stamp so [authn.RequireFreshStamp] (which the v0.2
// route stack composes) lets the request through. Without the real
// row, the SecurityStampValidator's read-through factory would 404
// on cache miss + the middleware would 401 — the freshness gate is
// not bypassable for synthetic operators.
//
// Pass operatorPersonID="" to auto-generate a UUIDv7. For tests that
// need a stable ID across calls (e.g. operator-isolation scenarios),
// pass an explicit ID; the helper still inserts a Person under that ID.
func (f e2eFixture) mintPlatformToken(t *testing.T, operatorPersonID string) string {
	t.Helper()
	if operatorPersonID == "" {
		operatorPersonID = ids.NewV7().String()
	}

	// Seed the operator Person row so the freshness validator can
	// resolve the security_stamp claim. Email + name are synthetic;
	// the password hash is a fixed-shape placeholder (operators don't
	// authenticate via password in v0.2 — they're seeded via JWT).
	suffix := operatorPersonID[len(operatorPersonID)-12:]
	addr, err := email.New("operator-" + suffix + "@platform.test")
	if err != nil {
		t.Fatalf("operator email: %v", err)
	}
	pwHash, err := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	)
	if err != nil {
		t.Fatalf("operator pw hash: %v", err)
	}
	op, err := person.New(person.ID(operatorPersonID), addr, "Platform", "Operator", pwHash, testNow)
	if err != nil {
		t.Fatalf("operator person.New: %v", err)
	}
	if err := f.persons.Add(t.Context(), op); err != nil {
		t.Fatalf("operator persons.Add: %v", err)
	}

	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID:      operatorPersonID,
		TenantID:      ids.NewV7().String(), // synthetic — Platform doesn't bind to a real tenant for these tests
		TenantSlug:    "platform",
		MembershipID:  ids.NewV7().String(),
		SecurityStamp: op.SecurityStamp().String(),
		IsPlatform:    true,
		Permissions: []string{
			permission.IdentityPermissions.Platform.TenantsView,
			permission.IdentityPermissions.Platform.TenantsManage,
			permission.IdentityPermissions.Platform.UsersView,
			permission.IdentityPermissions.Platform.UsersManage,
		},
	})
	if err != nil {
		t.Fatalf("mint platform token: %v", err)
	}
	return tok
}

// ----- HTTP helpers ---------------------------------------------------------

func (f e2eFixture) postJSON(t *testing.T, path string, body any) httpResp {
	t.Helper()
	return f.do(t, http.MethodPost, path, "", body)
}

func (f e2eFixture) authedJSON(t *testing.T, method, path, token string, body any) httpResp {
	t.Helper()
	return f.do(t, method, path, token, body)
}

func (f e2eFixture) do(t *testing.T, method, path, token string, body any) httpResp {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b := &bytes.Buffer{}
		if err := json.NewEncoder(b).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		buf = b
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.URL+path, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return httpResp{status: resp.StatusCode, body: respBody}
}

func decodeError(t *testing.T, body []byte) ports.ErrorResponse {
	t.Helper()
	var er ports.ErrorResponse
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("decode error: %v body=%s", err, body)
	}
	return er
}

// ----- Cross-tenant negatives (RLS + ownership gates) -----------------------

// TestE2E_CrossTenant_TenantRouteRejectedAsForbidden — Tenant A admin
// hitting /api/v1/tenants/{tenantBId}/profile. The
// RequireTenantContext middleware short-circuits at 403 BEFORE the
// repository sees the request — the JWT tenant_id claim doesn't
// match the URL path tenant.
func TestE2E_CrossTenant_TenantRouteRejectedAsForbidden(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	// Read attempt: A trying to read B → 403.
	read := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/"+tenantB.TenantID, tenantA.AccessToken, nil)
	if read.status != http.StatusForbidden {
		t.Errorf("GET cross-tenant: status %d, want 403; body=%s", read.status, read.body)
	}
	// Write attempt: A trying to rename B → 403.
	write := f.authedJSON(t, http.MethodPatch,
		"/api/v1/tenants/"+tenantB.TenantID+"/profile",
		tenantA.AccessToken,
		ports.UpdateTenantProfileRequest{LegalName: "hijacked", DisplayName: "h"})
	if write.status != http.StatusForbidden {
		t.Errorf("PATCH cross-tenant: status %d, want 403; body=%s", write.status, write.body)
	}
}

// TestE2E_CrossTenant_UserRouteHiddenAsNotFound — Tenant A admin
// hitting /api/v1/users/{tenantBMembershipId}. Per security.md
// enumeration-safety: cross-tenant access collapses to 404 (RLS
// silently filters at the DB level; the handler can't distinguish
// "wrong tenant" from "doesn't exist").
func TestE2E_CrossTenant_UserRouteHiddenAsNotFound(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	read := f.authedJSON(t, http.MethodGet,
		"/api/v1/users/"+tenantB.MembershipID, tenantA.AccessToken, nil)
	if read.status != http.StatusNotFound {
		t.Errorf("GET cross-tenant user: status %d, want 404; body=%s", read.status, read.body)
	}
	if er := decodeError(t, read.body); er.Error != ports.ErrCodeUserNotFound {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeUserNotFound)
	}

	patch := f.authedJSON(t, http.MethodPatch,
		"/api/v1/users/"+tenantB.MembershipID+"/profile",
		tenantA.AccessToken,
		ports.UpdateUserProfileRequest{Designation: "hijacked"})
	if patch.status != http.StatusNotFound {
		t.Errorf("PATCH cross-tenant user: status %d, want 404; body=%s", patch.status, patch.body)
	}

	deact := f.authedJSON(t, http.MethodPost,
		"/api/v1/users/"+tenantB.MembershipID+"/deactivate",
		tenantA.AccessToken,
		ports.DeactivateUserRequest{Reason: "cross-tenant attack"})
	if deact.status != http.StatusNotFound {
		t.Errorf("POST cross-tenant deactivate: status %d, want 404; body=%s", deact.status, deact.body)
	}

	// And confirm Tenant A's listing returns ONLY its own users
	// (not Tenant B's). This is the affirmative side of RLS — proves
	// LIST queries are correctly scoped.
	list := f.authedJSON(t, http.MethodGet, "/api/v1/users", tenantA.AccessToken, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list users: status %d body %s", list.status, list.body)
	}
	var lr ports.ListUsersResponse
	if err := json.Unmarshal(list.body, &lr); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(lr.Users) != 1 {
		t.Errorf("len(users) = %d, want 1 (only Tenant A's admin)", len(lr.Users))
	}
	for _, u := range lr.Users {
		if u.TenantID == tenantB.TenantID {
			t.Errorf("Tenant A's listing leaked Tenant B membership: %+v", u)
		}
	}
}

// TestE2E_CrossTenant_RoleRouteHiddenAsNotFound — Tenant A trying to
// read or mutate Tenant B's roles. Same RLS-collapse-to-404 rule.
func TestE2E_CrossTenant_RoleRouteHiddenAsNotFound(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	// Tenant B creates a custom role.
	createInB := f.authedJSON(t, http.MethodPost, "/api/v1/roles", tenantB.AccessToken,
		ports.CreateRoleRequest{Name: "B Custom Role", HierarchyLevel: 50})
	if createInB.status != http.StatusCreated {
		t.Fatalf("create role in B: status %d body %s", createInB.status, createInB.body)
	}
	var bRole ports.CreateRoleResponse
	if err := json.Unmarshal(createInB.body, &bRole); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Tenant A tries to read it → 404.
	read := f.authedJSON(t, http.MethodGet,
		"/api/v1/roles/"+bRole.RoleID, tenantA.AccessToken, nil)
	if read.status != http.StatusNotFound {
		t.Errorf("GET cross-tenant role: status %d, want 404; body=%s", read.status, read.body)
	}

	// Tenant A tries to delete it → 404.
	del := f.authedJSON(t, http.MethodDelete,
		"/api/v1/roles/"+bRole.RoleID, tenantA.AccessToken, nil)
	if del.status != http.StatusNotFound {
		t.Errorf("DELETE cross-tenant role: status %d, want 404; body=%s", del.status, del.body)
	}

	// Tenant A's listing must NOT include B's custom role. (System
	// defaults seeded per-tenant are local to A's tenant — those
	// rows are different identities even if they share a name.)
	list := f.authedJSON(t, http.MethodGet, "/api/v1/roles", tenantA.AccessToken, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list roles in A: status %d body %s", list.status, list.body)
	}
	var lr ports.ListRolesResponse
	if err := json.Unmarshal(list.body, &lr); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	for _, r := range lr.Roles {
		if r.ID == bRole.RoleID {
			t.Errorf("Tenant A's listing leaked Tenant B role %s", r.ID)
		}
	}
}

// TestE2E_CrossTenant_SessionRevokeBlocked — DELETE
// /api/v1/auth/sessions/{familyId} for a session owned by another
// Person. Per security.md enumeration-safety: cross-Person revocation
// collapses to 404 (NOT 403 — defeats family-id enumeration via
// ownership probing).
func TestE2E_CrossTenant_SessionRevokeBlocked(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	// Tenant A's admin lists their sessions.
	listA := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tenantA.AccessToken, nil)
	if listA.status != http.StatusOK {
		t.Fatalf("list sessions A: status %d body %s", listA.status, listA.body)
	}
	var sessA ports.ListSessionsResponse
	if err := json.Unmarshal(listA.body, &sessA); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessA.Sessions) != 1 {
		t.Fatalf("expected 1 session for A, got %d", len(sessA.Sessions))
	}
	aFamilyID := sessA.Sessions[0].FamilyID

	// Tenant B tries to revoke Tenant A's family → 404 (ownership gate).
	revoke := f.authedJSON(t, http.MethodDelete,
		"/api/v1/auth/sessions/"+aFamilyID, tenantB.AccessToken, nil)
	if revoke.status != http.StatusNotFound {
		t.Errorf("cross-Person revoke: status %d, want 404; body=%s", revoke.status, revoke.body)
	}

	// Tenant A's family must STILL be active (B's attempt was a no-op).
	listAAgain := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", tenantA.AccessToken, nil)
	var sessA2 ports.ListSessionsResponse
	_ = json.Unmarshal(listAAgain.body, &sessA2)
	if len(sessA2.Sessions) != 1 {
		t.Errorf("Tenant A's session count after attack = %d, want 1", len(sessA2.Sessions))
	}
}

// ----- Platform-tier negatives (tenant Admin reaching operator routes) ------

// TestE2E_PlatformGate_TenantAdminBlocked — every /api/v1/platform/...
// route MUST return 403 to a tenant-Admin token. Sweeps the most
// important platform endpoints in one test for breadth.
func TestE2E_PlatformGate_TenantAdminBlocked(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"ListAllTenants", http.MethodGet, "/api/v1/platform/tenants", nil},
		{"GetPerson", http.MethodGet, "/api/v1/platform/persons/" + tenantA.PersonID, nil},
		{"ListPersonMemberships", http.MethodGet,
			"/api/v1/platform/persons/" + tenantA.PersonID + "/memberships", nil},
		{"GlobalSuspendPerson", http.MethodPost,
			"/api/v1/platform/persons/" + tenantA.PersonID + "/global-suspend",
			ports.GlobalSuspendPersonRequest{Reason: "this is a 10+ char reason"}},
		{"SuspendTenant", http.MethodPost,
			"/api/v1/tenants/" + tenantA.TenantID + "/suspend",
			ports.SuspendTenantRequest{Reason: "this is a 10+ char reason"}},
		{"PlatformStats", http.MethodGet, "/api/v1/platform/stats", nil},
		{"CreateImpersonationSession", http.MethodPost,
			"/api/v1/platform/impersonation/sessions",
			ports.CreateImpersonationSessionRequest{
				TargetTenantID: tenantA.TenantID,
				Reason:         "diagnostic: cross-tenant audit work",
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := f.authedJSON(t, c.method, c.path, tenantA.AccessToken, c.body)
			if resp.status != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403; body=%s",
					c.method, c.path, resp.status, resp.body)
			}
		})
	}
}

// TestE2E_Auth_NoToken_Returns401 — authenticated routes without a
// Bearer token return 401, not 404 or 200.
func TestE2E_Auth_NoToken_Returns401(t *testing.T) {
	f := newE2EFixture(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/roles"},
		{http.MethodGet, "/api/v1/auth/sessions"},
		{http.MethodGet, "/api/v1/platform/tenants"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp := f.authedJSON(t, c.method, c.path, "", nil)
			if resp.status != http.StatusUnauthorized {
				t.Errorf("status: got %d, want 401; body=%s", resp.status, resp.body)
			}
		})
	}
}

// ----- Platform happy paths -------------------------------------------------

// TestE2E_PlatformOperator_BypassesTenantGate — a Platform-tier token
// can read/mutate any tenant's resources via /api/v1/tenants/{id}/...
// routes. This is the "is_platform=true" bypass the
// RequireTenantContext middleware honours.
func TestE2E_PlatformOperator_BypassesTenantGate(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")

	// Read any tenant.
	read := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/"+tenantA.TenantID, platformTok, nil)
	if read.status != http.StatusOK {
		t.Errorf("Platform GET tenant: status %d, want 200; body=%s", read.status, read.body)
	}

	// Suspend any tenant (Platform-only route).
	suspend := f.authedJSON(t, http.MethodPost,
		"/api/v1/tenants/"+tenantA.TenantID+"/suspend",
		platformTok,
		ports.SuspendTenantRequest{Reason: "compliance hold 2026-05-07"})
	if suspend.status != http.StatusNoContent {
		t.Errorf("Platform suspend: status %d, want 204; body=%s", suspend.status, suspend.body)
	}

	// Activate again — round-trip lifecycle.
	activate := f.authedJSON(t, http.MethodPost,
		"/api/v1/tenants/"+tenantA.TenantID+"/activate",
		platformTok, nil)
	if activate.status != http.StatusNoContent {
		t.Errorf("Platform activate: status %d, want 204; body=%s", activate.status, activate.body)
	}
}

// TestE2E_PlatformOperator_ListsAllTenants — operator's
// /api/v1/platform/tenants returns BOTH tenants (cross-tenant view).
func TestE2E_PlatformOperator_ListsAllTenants(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")
	platformTok := f.mintPlatformToken(t, "")

	resp := f.authedJSON(t, http.MethodGet, "/api/v1/platform/tenants", platformTok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("list all tenants: status %d body %s", resp.status, resp.body)
	}
	var lr ports.ListAllTenantsResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seenA, seenB := false, false
	for _, t := range lr.Tenants {
		if t.ID == tenantA.TenantID {
			seenA = true
		}
		if t.ID == tenantB.TenantID {
			seenB = true
		}
	}
	if !seenA || !seenB {
		t.Errorf("ListAllTenants missing one of the registered tenants: seenA=%v seenB=%v", seenA, seenB)
	}
}

// TestE2E_PlatformStats_ReflectsState — counts match the registered
// tenants + memberships.
//
// Per-helper contribution to the stats counters (encoded as a contract
// the helpers + assertions share, not magic numbers in the test body):
//
//   registerAndLogin → +1 tenant, +1 person, +1 active membership
//   mintPlatformToken → +1 person (the synthetic operator Person);
//                       no tenant, no membership (TenantID claim is
//                       synthetic, no DB row)
func TestE2E_PlatformStats_ReflectsState(t *testing.T) {
	f := newE2EFixture(t)
	admins := []registeredTenant{
		f.registerAndLogin(t, "acme"),
		f.registerAndLogin(t, "globex"),
	}
	operators := []string{f.mintPlatformToken(t, "")}

	wantTenants := len(admins)
	wantPersons := len(admins) + len(operators)
	wantActiveMemberships := len(admins)

	resp := f.authedJSON(t, http.MethodGet, "/api/v1/platform/stats", operators[0], nil)
	if resp.status != http.StatusOK {
		t.Fatalf("stats: status %d body %s", resp.status, resp.body)
	}
	var stats ports.PlatformStatsResponse
	if err := json.Unmarshal(resp.body, &stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.TenantsTotal != wantTenants {
		t.Errorf("TenantsTotal = %d, want %d (registerAndLogin × %d)",
			stats.TenantsTotal, wantTenants, len(admins))
	}
	if stats.PersonsTotal != wantPersons {
		t.Errorf("PersonsTotal = %d, want %d (registerAndLogin × %d + mintPlatformToken × %d)",
			stats.PersonsTotal, wantPersons, len(admins), len(operators))
	}
	if stats.MembershipsActive != wantActiveMemberships {
		t.Errorf("MembershipsActive = %d, want %d (registerAndLogin × %d)",
			stats.MembershipsActive, wantActiveMemberships, len(admins))
	}
}

// ----- Single-Active-Membership invariant -----------------------------------

// TestE2E_RegisterTenant_SameAdminEmailRejected — registering a
// SECOND tenant with an admin email that already has an Active
// Membership → 409 email_has_active_membership. Per
// multi-tenancy.md "Identity model: at most one Active Membership
// per Person" + the partial unique index.
func TestE2E_RegisterTenant_SameAdminEmailRejected(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	// Same email, different slug → second register attempt MUST fail.
	suffix := ids.NewV7().String()[len(ids.NewV7().String())-8:]
	resp := f.postJSON(t, "/api/v1/tenants", ports.RegisterTenantRequest{
		Slug:           "globex-" + suffix,
		LegalName:      "Globex Pharma Pvt Ltd",
		DisplayName:    "Globex",
		AdminEmail:     tenantA.Email, // collision
		AdminPassword:  "different-password-still-rejected",
		AdminFirstName: "Globex",
		AdminLastName:  "Admin",
	})
	if resp.status != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeEmailHasActiveMembership {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeEmailHasActiveMembership)
	}
}

// ----- Self-service positives (sanity that authentic happy paths work) ------

// TestE2E_TenantAdmin_SelfServicePath — Admin reads + updates own
// tenant + own memberships + lists own roles. Affirmative path that
// proves the new endpoints work end-to-end through real DB +
// adapter, not just unit-test fakes.
func TestE2E_TenantAdmin_SelfServicePath(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	// GET own tenant.
	read := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/"+tenantA.TenantID, tenantA.AccessToken, nil)
	if read.status != http.StatusOK {
		t.Fatalf("GET own tenant: %d body %s", read.status, read.body)
	}

	// PATCH profile.
	patch := f.authedJSON(t, http.MethodPatch,
		"/api/v1/tenants/"+tenantA.TenantID+"/profile",
		tenantA.AccessToken,
		ports.UpdateTenantProfileRequest{
			LegalName: "Acme Renamed Pvt Ltd", DisplayName: "Acme R",
		})
	if patch.status != http.StatusNoContent {
		t.Errorf("PATCH profile: %d body %s", patch.status, patch.body)
	}

	// GET own membership.
	getU := f.authedJSON(t, http.MethodGet,
		"/api/v1/users/"+tenantA.MembershipID, tenantA.AccessToken, nil)
	if getU.status != http.StatusOK {
		t.Errorf("GET own membership: %d body %s", getU.status, getU.body)
	}

	// PATCH own profile.
	patchU := f.authedJSON(t, http.MethodPatch,
		"/api/v1/users/"+tenantA.MembershipID+"/profile",
		tenantA.AccessToken,
		ports.UpdateUserProfileRequest{
			Designation: "Founder", Department: "Executive",
		})
	if patchU.status != http.StatusNoContent {
		t.Errorf("PATCH own profile: %d body %s", patchU.status, patchU.body)
	}

	// GET roles list.
	roles := f.authedJSON(t, http.MethodGet, "/api/v1/roles", tenantA.AccessToken, nil)
	if roles.status != http.StatusOK {
		t.Errorf("GET roles: %d body %s", roles.status, roles.body)
	}
}

// ----- Impersonation lifecycle ----------------------------------------------

// TestE2E_Impersonation_Lifecycle — create + list + end. Single
// operator, single session.
func TestE2E_Impersonation_Lifecycle(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")

	create := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		platformTok,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID:  tenantA.TenantID,
			Reason:          "diagnostic: investigating ticket TICKET-1234",
			DurationMinutes: 30,
		})
	if create.status != http.StatusCreated {
		t.Fatalf("create: %d body %s", create.status, create.body)
	}
	var cr ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(create.body, &cr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cr.SessionID == "" {
		t.Fatal("empty session id")
	}

	// List — must include this session.
	list := f.authedJSON(t, http.MethodGet,
		"/api/v1/platform/impersonation/sessions", platformTok, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list: %d body %s", list.status, list.body)
	}
	var lr ports.ListImpersonationSessionsResponse
	if err := json.Unmarshal(list.body, &lr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, s := range lr.Sessions {
		if s.SessionID == cr.SessionID {
			found = true
			if s.TargetTenantID != tenantA.TenantID {
				t.Errorf("TargetTenantID = %q, want %q", s.TargetTenantID, tenantA.TenantID)
			}
		}
	}
	if !found {
		t.Errorf("session %s not in list", cr.SessionID)
	}

	// End.
	end := f.authedJSON(t, http.MethodDelete,
		"/api/v1/platform/impersonation/sessions/"+cr.SessionID, platformTok, nil)
	if end.status != http.StatusNoContent {
		t.Errorf("end: %d body %s", end.status, end.body)
	}

	// Listing again — empty.
	list2 := f.authedJSON(t, http.MethodGet,
		"/api/v1/platform/impersonation/sessions", platformTok, nil)
	var lr2 ports.ListImpersonationSessionsResponse
	_ = json.Unmarshal(list2.body, &lr2)
	for _, s := range lr2.Sessions {
		if s.SessionID == cr.SessionID {
			t.Error("ended session still appears in list")
		}
	}
}

// TestE2E_Impersonation_RejectsShortReason — reason < 10 chars
// must fail at 422 impersonation_invalid per the session VO's
// audit gate.
func TestE2E_Impersonation_RejectsShortReason(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	platformTok := f.mintPlatformToken(t, "")

	resp := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		platformTok,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID: tenantA.TenantID,
			Reason:         "short",
		})
	if resp.status != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d, want 422; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeImpersonationInvalid {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeImpersonationInvalid)
	}
}

// TestE2E_Impersonation_OperatorIsolation — operator A creates a
// session; operator B lists their own → does NOT see A's session.
func TestE2E_Impersonation_OperatorIsolation(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	operatorA := f.mintPlatformToken(t, ids.NewV7().String())
	operatorB := f.mintPlatformToken(t, ids.NewV7().String())

	// A creates a session.
	create := f.authedJSON(t, http.MethodPost,
		"/api/v1/platform/impersonation/sessions",
		operatorA,
		ports.CreateImpersonationSessionRequest{
			TargetTenantID: tenantA.TenantID,
			Reason:         "diagnostic: legitimate work session",
		})
	if create.status != http.StatusCreated {
		t.Fatalf("create: %d body %s", create.status, create.body)
	}
	var cr ports.CreateImpersonationSessionResponse
	_ = json.Unmarshal(create.body, &cr)

	// B lists their sessions — must not see A's.
	list := f.authedJSON(t, http.MethodGet,
		"/api/v1/platform/impersonation/sessions", operatorB, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list: %d body %s", list.status, list.body)
	}
	var lr ports.ListImpersonationSessionsResponse
	_ = json.Unmarshal(list.body, &lr)
	for _, s := range lr.Sessions {
		if s.SessionID == cr.SessionID {
			t.Errorf("operator B saw operator A's session %s", s.SessionID)
		}
	}
}

// ----- Auth flow negatives --------------------------------------------------

// TestE2E_ChangePassword_RejectsWrongCurrent — authenticated, but
// supplies wrong current password → 401 incorrect_current_password.
func TestE2E_ChangePassword_RejectsWrongCurrent(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	resp := f.authedJSON(t, http.MethodPost,
		"/api/v1/auth/change-password", tenantA.AccessToken,
		ports.ChangePasswordRequest{
			CurrentPassword: "totally-wrong-password",
			NewPassword:     "Tr0ub4dor&3-newly-strong",
		})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeIncorrectCurrentPassword {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeIncorrectCurrentPassword)
	}
}

// TestE2E_ResetPassword_BadTokenRejected — submitting reset-password
// with an invalid token → 400 reset_token_invalid. Anonymous endpoint.
func TestE2E_ResetPassword_BadTokenRejected(t *testing.T) {
	f := newE2EFixture(t)
	resp := f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{
		Token:       "totally-bogus-token-not-issued-by-us",
		NewPassword: "Tr0ub4dor&3-newly-strong",
	})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeResetTokenInvalid {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeResetTokenInvalid)
	}
}

// TestE2E_RequestPasswordReset_SilentSuccess — unknown email → 204
// (Auth0/Okta canon: never disclose account existence). Same wire
// shape as known-email path.
func TestE2E_RequestPasswordReset_SilentSuccess(t *testing.T) {
	f := newE2EFixture(t)
	resp := f.postJSON(t, "/api/v1/auth/request-password-reset",
		ports.RequestPasswordResetRequest{
			Email: fmt.Sprintf("never-registered-%s@nowhere.test", ids.NewV7().String()),
		})
	if resp.status != http.StatusNoContent {
		t.Errorf("status: got %d, want 204 silent success; body=%s", resp.status, resp.body)
	}
}
