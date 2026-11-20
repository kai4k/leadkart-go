//go:build integration

// arch-test:no-timeout-needed — newE2EFixture* → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).
//
// arch-test:no-synctest — the cascade tests cross the real forwarder, router and
// Postgres; a virtual clock cannot order them.

// Hierarchy round 4, I2: the ACTOR of an authority command is decided on facts
// read inside the locked transaction, not on the rank and permissions their
// access token carried when it was minted (up to ten minutes ago). A token can
// only ever lose authority relative to the stored facts, never gain it.
//
// The first group runs WITHOUT the invalidation cascade and with the stamp
// cache warmed, i.e. the worst case of the 30 s stamp-cache window: the
// demoted member's old token still reaches the command, which must refuse it.
// The second group runs WITH the cascade and asserts what the BFF sees: the old
// token answers 401 stale_token (the signal that triggers a silent refresh),
// and the refreshed token carries the new rank and permissions.

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// loginPair logs in and returns the access AND refresh token.
func (f e2eFixture) loginPair(t *testing.T, email, password string) ports.LoginResponse {
	t.Helper()
	identitytest.ClearMustChangePassword(t, f.Pool, email)
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

// warmStamp makes one authenticated request so the stamp cache holds the
// person's CURRENT stamp: from now on the token is accepted for the cache TTL
// even if the stamp rotates and nothing evicts the cache.
func (f e2eFixture) warmStamp(t *testing.T, token string) {
	t.Helper()
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", token, nil); r.status != http.StatusOK {
		t.Fatalf("warm stamp cache: %d %s", r.status, r.body)
	}
}

// tokenClaims decodes the (unverified) payload of a JWT the test just received.
func tokenClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode claims json: %v", err)
	}
	return m
}

// TestE2E_StaleActor_DemotedOwnerCannotMintAnOwner is the demoted-owner
// exploit: O2 loses Company Owner (or is deactivated) while their token still
// says rank 0 with tenant.admin. Using that token, O2 creates a helper account,
// makes it Company Owner, logs in as it and has it hand ownership back.
func TestE2E_StaleActor_DemotedOwnerCannotMintAnOwner(t *testing.T) {
	variants := map[string]func(t *testing.T, f e2eFixture, o1 registeredTenant, o2 createdMember, ownerRole string){
		"owner role revoked": func(t *testing.T, f e2eFixture, o1 registeredTenant, o2 createdMember, ownerRole string) {
			if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+o2.membershipID+"/roles/"+ownerRole, o1.AccessToken, nil); r.status != http.StatusNoContent {
				t.Fatalf("demote O2: %d %s", r.status, r.body)
			}
		},
		"deactivated": func(t *testing.T, f e2eFixture, o1 registeredTenant, o2 createdMember, _ string) {
			if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o2.membershipID+"/deactivate", o1.AccessToken,
				ports.DeactivateUserRequest{Reason: "stale actor test"}); r.status != http.StatusNoContent {
				t.Fatalf("deactivate O2: %d %s", r.status, r.body)
			}
		},
	}
	for name, demote := range variants {
		// arch-test:serial — subtests each start their own fixture; run one at a time to bound Docker load.
		t.Run(name, func(t *testing.T) {
			f := newE2EFixtureAt(t, time.Now)
			o1 := f.registerAndLogin(t, "stale-owner")
			ownerRole := f.roleIDByName(t, o1.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
			o2 := f.addSecondOwner(t, o1, "o2")
			t2 := f.loginToken(t, o2.email, o2.password)
			f.warmStamp(t, t2)
			demote(t, f, o1, o2, ownerRole)

			// Step 1: the helper account. It carries no authority (CreateUser
			// writes none), so it is allowed to succeed or be refused.
			create := f.authedJSON(t, http.MethodPost, "/api/v1/users", t2,
				ports.CreateUserRequest{Email: "helper-" + o2.membershipID[24:] + "@e2e.test", Password: "Helper-Pass-2026!x", FirstName: "Helper", LastName: "Acct"})
			if create.status == http.StatusUnauthorized {
				return // the stale token was refused outright: the exploit ends here
			}
			if create.status != http.StatusCreated {
				t.Fatalf("create helper: %d %s", create.status, create.body)
			}
			var cu ports.CreateUserResponse
			if err := json.Unmarshal(create.body, &cu); err != nil {
				t.Fatalf("decode helper: %v", err)
			}
			// Step 2: the stale actor makes the helper a Company Owner.
			r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+cu.MembershipID+"/roles", t2, ports.AssignUserRoleRequest{RoleID: ownerRole})
			t.Logf("stale owner assigns Company Owner: %d %s", r.status, r.body)
			if r.status == http.StatusNoContent {
				// Step 3 of the exploit: the helper hands ownership back.
				helperTok := f.loginToken(t, "helper-"+o2.membershipID[24:]+"@e2e.test", "Helper-Pass-2026!x")
				back := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+o2.membershipID+"/roles", helperTok, ports.AssignUserRoleRequest{RoleID: ownerRole})
				t.Fatalf("TAKEOVER: a demoted owner's stale token made a new Company Owner (204); the helper then re-assigned ownership: %d %s", back.status, back.body)
			}
			if r.status != http.StatusForbidden && r.status != http.StatusUnauthorized {
				t.Fatalf("stale owner assigns Company Owner: %d %s, want 403 or 401", r.status, r.body)
			}
			if u := f.getUser(t, o1.AccessToken, cu.MembershipID); len(u.RoleIDs) != 0 {
				t.Errorf("the helper account holds roles %v after the refused assignment", u.RoleIDs)
			}
		})
	}
}

// TestE2E_StaleActor_DemotedAdminCannotConferWhatTheyNoLongerOutrank covers the
// admin variant: the member was an Administrator (rank 10) and is now a Sales
// Manager (rank 20); the old token still says 10.
func TestE2E_StaleActor_DemotedAdminCannotConferWhatTheyNoLongerOutrank(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "stale-admin")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	admin := f.createMember(t, owner, "admin")
	f.assignRole(t, owner.AccessToken, admin.membershipID, roleID(role.SystemRoles.Tenant.Administrator))
	adminTok := f.loginToken(t, admin.email, admin.password)
	f.warmStamp(t, adminTok)
	helper := f.createMember(t, owner, "helper")

	// The owner demotes the admin to Sales Manager.
	if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+admin.membershipID+"/roles/"+roleID(role.SystemRoles.Tenant.Administrator), owner.AccessToken, nil); r.status != http.StatusNoContent {
		t.Fatalf("revoke Administrator: %d %s", r.status, r.body)
	}
	f.assignRole(t, owner.AccessToken, admin.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))

	// A level-15 role was within the old rank 10; it is not within rank 20.
	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+helper.membershipID+"/roles", adminTok,
		ports.AssignUserRoleRequest{RoleID: roleID(role.SystemRoles.Tenant.SeniorManager)})
	f.wantForbidden(t, "demoted admin assigns Senior Manager", r, http.StatusForbidden, "insufficient_rank")
	if u := f.getUser(t, owner.AccessToken, helper.membershipID); len(u.RoleIDs) != 0 {
		t.Errorf("the helper holds roles %v after refused assignments", u.RoleIDs)
	}
}

// TestE2E_StaleActor_RevokedPermissionCannotBeConferred: the token still lists
// a permission an override conferred a minute ago; the intersection with the
// freshly resolved set removes it.
func TestE2E_StaleActor_RevokedPermissionCannotBeConferred(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "stale-perm")
	p := permission.IdentityPermissions
	mgr := f.createMember(t, owner, "mgr")
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesManager))
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	exec2 := f.createMember(t, owner, "exec2")
	f.assignRole(t, owner.AccessToken, exec2.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	overrides := func(granted, revoked []string) {
		t.Helper()
		if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+mgr.membershipID+"/permission-overrides", owner.AccessToken,
			ports.ReplaceUserPermissionOverridesRequest{Granted: granted, Revoked: revoked}); r.status != http.StatusNoContent {
			t.Fatalf("overrides: %d %s", r.status, r.body)
		}
	}
	// The manager may change overrides, and holds identity.users.view ONLY through the override.
	overrides([]string{p.Users.UpdatePermissions, p.Users.View}, []string{})
	mgrTok := f.loginToken(t, mgr.email, mgr.password)
	f.warmStamp(t, mgrTok)
	// The manager legitimately confers it while they hold it.
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+exec.membershipID+"/permission-overrides", mgrTok,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Users.View}, Revoked: []string{}}); r.status != http.StatusNoContent {
		t.Fatalf("sanity: manager confers a permission they hold: %d %s", r.status, r.body)
	}
	// The owner takes identity.users.view away from the manager (the update_permissions grant stays).
	overrides([]string{p.Users.UpdatePermissions}, []string{p.Users.View})
	// The stale token still lists identity.users.view; conferring it again must be refused.
	r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+exec2.membershipID+"/permission-overrides", mgrTok,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Users.View, p.Crm.Leads.Read}, Revoked: []string{}})
	f.wantForbidden(t, "manager confers a permission revoked from them a minute ago", r, http.StatusForbidden, "grant_exceeds_authority")
}

// TestE2E_StaleActor_RoleChangeAnswersStaleTokenAndRefreshMintsTheNewRank is
// what the BFF sees (cascade wired, as in production). After an authority change
// the affected member's NEXT request answers 401 stale_token with
// WWW-Authenticate — the answer the BFF turns into a silent refresh, not a
// sign-out — and the refreshed token carries the new rank.
func TestE2E_StaleActor_RoleChangeAnswersStaleTokenAndRefreshMintsTheNewRank(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	owner := f.registerAndLogin(t, "stale-answer")
	mgr := f.createMember(t, owner, "promoted")
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesManager))
	pair := f.loginPair(t, mgr.email, mgr.password)
	f.warmStamp(t, pair.AccessToken)
	if got := tokenClaims(t, pair.AccessToken)["rank"]; got != float64(20) {
		t.Fatalf("manager token rank claim = %v, want 20", got)
	}

	// Promotion: the owner makes the manager an Administrator.
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.Administrator))

	stale := f.untilStale(t, pair.AccessToken)
	if stale.status != http.StatusUnauthorized || decodeError(t, stale.body).Error != "stale_token" {
		t.Fatalf("old token after a role change: %d %s, want 401 stale_token", stale.status, stale.body)
	}
	if got := stale.header.Get("WWW-Authenticate"); got != `Bearer realm="leadkart"` {
		t.Errorf("WWW-Authenticate = %q, want the Bearer challenge the BFF keys the silent refresh on", got)
	}

	// The BFF's silent refresh: the refresh token is still valid and re-mints from fresh roles.
	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	if rr.status != http.StatusOK {
		t.Fatalf("refresh after a role change: %d %s, want 200 (the session must survive)", rr.status, rr.body)
	}
	var fresh ports.LoginResponse
	if err := json.Unmarshal(rr.body, &fresh); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if got := tokenClaims(t, fresh.AccessToken)["rank"]; got != float64(10) {
		t.Errorf("refreshed token rank claim = %v, want 10 (Administrator)", got)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users?page_size=5", fresh.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("refreshed token: %d %s, want 200", r.status, r.body)
	}
}

// untilStale polls (bounded) until the token's next request answers something
// other than 200, the cascade being asynchronous, and returns that answer.
func (f e2eFixture) untilStale(t *testing.T, token string) httpResp {
	t.Helper()
	deadline := time.Now().Add(invalidationFastPathBudget)
	var r httpResp
	// arch-test:wait-justified — bounded poll for the async stamp-cache eviction; the path needs the real forwarder, router and DB.
	for time.Now().Before(deadline) {
		r = f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", token, nil)
		if r.status != http.StatusOK {
			return r
		}
		time.Sleep(50 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop awaiting the async cascade
	}
	return r
}

// TestE2E_StaleActor_RevokedPermissionIsRefusedByOtherModulesAtOnce: a
// permission removed by override is no longer honoured by another module's
// route on the very next request (no ten-minute window).
func TestE2E_StaleActor_RevokedPermissionIsRefusedByOtherModulesAtOnce(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	owner := f.registerAndLogin(t, "stale-module")
	mgr := f.createMember(t, owner, "inv")
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.PurchaseManager))
	pair := f.loginPair(t, mgr.email, mgr.password)
	products := func(token string) int {
		return f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products", token, nil).status
	}
	if s := products(pair.AccessToken); s != http.StatusOK {
		t.Fatalf("sanity: Purchase Manager reads the catalogue: %d", s)
	}
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+mgr.membershipID+"/permission-overrides", owner.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{"inventory.catalog.read", "inventory.catalog.manage"}}); r.status != http.StatusNoContent {
		t.Fatalf("revoke inventory.catalog.read: %d %s", r.status, r.body)
	}
	if s := f.untilStale(t, pair.AccessToken); s.status != http.StatusUnauthorized {
		t.Fatalf("revoked permission, old token: %d %s, want 401 stale_token (it used to keep working for up to ten minutes)", s.status, s.body)
	}
	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	if rr.status != http.StatusOK {
		t.Fatalf("refresh: %d %s", rr.status, rr.body)
	}
	var fresh ports.LoginResponse
	if err := json.Unmarshal(rr.body, &fresh); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if got := products(fresh.AccessToken); got != http.StatusForbidden {
		t.Errorf("refreshed token reads the catalogue: %d, want 403", got)
	}
	if perms, _ := tokenClaims(t, fresh.AccessToken)["permission"].([]any); slices.Contains(perms, any("inventory.catalog.read")) {
		t.Errorf("refreshed token still carries inventory.catalog.read")
	}
}

// TestE2E_StaleActor_DeactivatedMemberIsSignedOut: deactivation ends the
// session for real — the old token is stale AND the refresh is refused.
func TestE2E_StaleActor_DeactivatedMemberIsSignedOut(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	owner := f.registerAndLogin(t, "stale-deact")
	m := f.createMember(t, owner, "leaver")
	f.assignRole(t, owner.AccessToken, m.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	pair := f.loginPair(t, m.email, m.password)
	f.warmStamp(t, pair.AccessToken)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", owner.AccessToken,
		ports.DeactivateUserRequest{Reason: "leaving"}); r.status != http.StatusNoContent {
		t.Fatalf("deactivate: %d %s", r.status, r.body)
	}
	if s := f.untilStale(t, pair.AccessToken); s.status != http.StatusUnauthorized {
		t.Fatalf("deactivated member's old token: %d %s, want 401", s.status, s.body)
	}
	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	if rr.status == http.StatusOK {
		t.Fatalf("a deactivated member refreshed their session: %d %s", rr.status, rr.body)
	}
}
