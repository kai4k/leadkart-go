//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// Plan D8 (ADR 0053 deferred work #1) — the API enforces must_change_password.
// A person provisioned with a temporary credential (RegisterTenant's admin, a
// member created with POST /users) gets a token carrying the flag; that token
// reaches change-password and the caller's own capabilities and nothing else,
// until the password is changed. The front-end used to enforce this with a
// cookie the client could drop.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) rawLogin(t *testing.T, email, password string) ports.LoginResponse {
	t.Helper()
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: email, Password: password, DeviceLabel: "e2e"})
	if resp.status != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, resp.status, resp.body)
	}
	var lr ports.LoginResponse
	if err := json.Unmarshal(resp.body, &lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return lr
}

func (f e2eFixture) requirePasswordChangeRequired(t *testing.T, what, method, path, token string, body any) {
	t.Helper()
	r := f.authedJSON(t, method, path, token, body)
	if r.status != http.StatusForbidden || errCodeOf(r) != "password_change_required" {
		t.Errorf("%s: %s %s answered %d %s; want 403 password_change_required", what, method, path, r.status, r.body)
	}
	if r.header.Get("WWW-Authenticate") != "" {
		t.Errorf("%s: a 403 must not carry WWW-Authenticate (the BFF reads 401 + the header as 'refresh')", what)
	}
}

func TestE2E_MustChangePassword_BlocksEveryRouteUntilThePasswordIsChanged(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerOnly(t, "mcp") // the admin holds the temporary credential
	login := f.rawLogin(t, reg.Email, reg.Password)
	if !login.MustChangePassword {
		t.Fatal("login must report must_change_password for a temporary credential")
	}
	claims, err := f.Issuer.Verify(login.AccessToken)
	if err != nil || !claims.MustChangePassword {
		t.Fatalf("the access token must carry the claim: %v %+v", err, claims)
	}

	// Blocked: an identity read, another module's read, an authority command, a platform route.
	f.requirePasswordChangeRequired(t, "identity read", http.MethodGet, "/api/v1/users", login.AccessToken, nil)
	f.requirePasswordChangeRequired(t, "another module", http.MethodGet, "/api/v1/inventory/products", login.AccessToken, nil)
	f.requirePasswordChangeRequired(t, "authority command", http.MethodPost, "/api/v1/users/"+reg.MembershipID+"/roles", login.AccessToken,
		ports.AssignUserRoleRequest{RoleID: "0190a000-0000-7000-8000-0000000000cc"})
	f.requirePasswordChangeRequired(t, "self-service", http.MethodGet, "/api/v1/auth/sessions", login.AccessToken, nil)

	// Allowlisted: the caller's own capabilities, which report the flag.
	caps := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", login.AccessToken, nil)
	var cd ports.CapabilitiesDto
	if err := json.Unmarshal(caps.body, &cd); err != nil || caps.status != http.StatusOK || !cd.MustChangePassword {
		t.Fatalf("capabilities: %d %s (must be 200 with must_change_password=true)", caps.status, caps.body)
	}
	// The forced-change screen shows the rules that bind the new password: the
	// caller's own tenant's policy (here the system default).
	if want := (ports.PasswordRulesDto{MinLength: 12, MaxLength: 1024, RequiresUppercase: true, RequiresLowercase: true, RequiresDigit: true, RequiresSymbol: true}); cd.PasswordPolicy != want {
		t.Fatalf("capabilities.password_policy = %+v, want %+v", cd.PasswordPolicy, want)
	}

	// Refresh keeps the flag: in the response and in the new token.
	ref := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: login.RefreshToken})
	var rr ports.LoginResponse
	if err := json.Unmarshal(ref.body, &rr); err != nil || ref.status != http.StatusOK {
		t.Fatalf("refresh: %d %s", ref.status, ref.body)
	}
	if !rr.MustChangePassword {
		t.Fatalf("refresh must carry must_change_password forward: %s", ref.body)
	}
	if c, err := f.Issuer.Verify(rr.AccessToken); err != nil || !c.MustChangePassword {
		t.Fatalf("the refreshed token must carry the claim: %v", err)
	}
	f.requirePasswordChangeRequired(t, "after refresh", http.MethodGet, "/api/v1/users", rr.AccessToken, nil)

	// Change password: the allowlisted way out. The old token dies (stamp rotated).
	newPw := "Rotated-Pass-2026!mcp"
	if cp := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", rr.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: reg.Password, NewPassword: newPw}); cp.status != http.StatusNoContent {
		t.Fatalf("change-password: %d %s", cp.status, cp.body)
	}
	f.evictStamp(t, reg.PersonID)
	old := f.authedJSON(t, http.MethodGet, "/api/v1/users", rr.AccessToken, nil)
	if old.status != http.StatusUnauthorized || errCodeOf(old) != "stale_token" {
		t.Fatalf("the flagged token after the change: %d %s; want 401 stale_token", old.status, old.body)
	}

	// A fresh login has no flag and works everywhere.
	fresh := f.rawLogin(t, reg.Email, newPw)
	if fresh.MustChangePassword {
		t.Fatal("login after the change must not report the flag")
	}
	if c, err := f.Issuer.Verify(fresh.AccessToken); err != nil || c.MustChangePassword {
		t.Fatalf("the post-change token must not carry the claim: %v", err)
	}
	for _, path := range []string{"/api/v1/users", "/api/v1/inventory/products", "/api/v1/auth/sessions"} {
		if r := f.authedJSON(t, http.MethodGet, path, fresh.AccessToken, nil); r.status != http.StatusOK {
			t.Errorf("after the change GET %s: %d %s", path, r.status, r.body)
		}
	}
	// And so does refresh from the fresh session.
	ref = f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: fresh.RefreshToken})
	if err := json.Unmarshal(ref.body, &rr); err != nil || ref.status != http.StatusOK || rr.MustChangePassword {
		t.Fatalf("refresh after the change: %d %s; want 200 without the flag", ref.status, ref.body)
	}
}

// A member created by an admin holds a temporary credential too; the owner's own
// token is not affected by the member's flag.
func TestE2E_MustChangePassword_AdminCreatedMemberIsFlaggedUntilItChangesIt(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "mcpm")
	m := f.createMember(t, owner, "invitee")

	login := f.rawLogin(t, m.email, m.password)
	if !login.MustChangePassword {
		t.Fatalf("an admin-created member must be flagged: %+v", login)
	}
	f.requirePasswordChangeRequired(t, "member", http.MethodGet, "/api/v1/users", login.AccessToken, nil)
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", owner.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("the owner's token must not be affected by the member's flag: %d %s", r.status, r.body)
	}
	if cp := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", login.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: m.password, NewPassword: "Member-Rotated-2026!x"}); cp.status != http.StatusNoContent {
		t.Fatalf("change-password: %d %s", cp.status, cp.body)
	}
}

// Tokens minted before the claim existed carry no flag and keep working, even for
// a person who is still flagged in the database: the claim is what is enforced.
func TestE2E_MustChangePassword_TokenWithoutTheClaimIsUnflagged(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerOnly(t, "mcpold")
	login := f.rawLogin(t, reg.Email, reg.Password)
	c, err := f.Issuer.Verify(login.AccessToken)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	old, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID: c.Subject, TenantID: c.TenantID, TenantSlug: c.TenantSlug, MembershipID: c.MembershipID,
		SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, c.Subject), Permissions: c.Permissions,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", old, nil); r.status != http.StatusOK {
		t.Fatalf("a token without the claim: %d %s; want 200", r.status, r.body)
	}
}

// The flag belongs to the person behind the token. An operator impersonating a
// tenant whose admin is still flagged is not blocked by the TENANT user's flag
// (the impersonation token is minted from the operator and never carries it); an
// operator who is flagged themselves cannot open a session at all.
func TestE2E_MustChangePassword_ImpersonationFollowsTheOperatorNotTheTenantUser(t *testing.T) {
	f := newE2EFixture(t)
	target := f.registerOnly(t, "mcpimp") // the tenant admin is still flagged
	platformTok := f.mintPlatformToken(t, "")
	sess := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", platformTok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: target.TenantID, Reason: "ticket 4711 forced change e2e", DurationMinutes: 30})
	if sess.status != http.StatusCreated {
		t.Fatalf("create session: %d %s", sess.status, sess.body)
	}
	var out ports.CreateImpersonationSessionResponse
	if err := json.Unmarshal(sess.body, &out); err != nil {
		t.Fatal(err)
	}
	if c, err := f.Issuer.Verify(out.AccessToken); err != nil || c.MustChangePassword {
		t.Fatalf("the impersonation token must not carry the flag: %v", err)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", out.AccessToken, nil); r.status != http.StatusOK {
		t.Fatalf("impersonation on a tenant with a flagged admin: %d %s; want 200", r.status, r.body)
	}

	// A flagged operator token is blocked like any other, platform routes included.
	flaggedOp, err := f.Issuer.Verify(platformTok)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID: flaggedOp.Subject, TenantID: flaggedOp.TenantID, TenantSlug: flaggedOp.TenantSlug, MembershipID: flaggedOp.MembershipID,
		SecurityStamp: flaggedOp.SecurityStamp, IsPlatform: true, Permissions: flaggedOp.Permissions, MustChangePassword: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.requirePasswordChangeRequired(t, "flagged operator", http.MethodPost, "/api/v1/platform/impersonation/sessions", tok,
		ports.CreateImpersonationSessionRequest{TargetTenantID: target.TenantID, Reason: "ticket 4712 flagged operator", DurationMinutes: 30})
}
