//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// ADR 0069 H2: every membership command acts only DOWNWARD the rank ladder.
// A caller whose ROUTE permission passes (here: an Administrator, or a
// Manager given identity.users.update by override) must still be refused
// when the target is not strictly more junior. One scenario per command,
// through the real mux.

package main

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) wantForbidden(t *testing.T, what string, r httpResp, status int, code string) {
	t.Helper()
	if r.status != status || decodeError(t, r.body).Error != code {
		t.Errorf("%s: %d %s, want %d %s", what, r.status, r.body, status, code)
	}
}

// TestE2E_MembershipCommands_ActOnlyDownTheLadder: each command is refused
// with insufficient_rank when the actor does not outrank the target, and the
// refused call changes nothing.
func TestE2E_MembershipCommands_ActOnlyDownTheLadder(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "ladder")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	ownerRole := roleID(role.SystemRoles.Tenant.CompanyOwner)
	p := permission.IdentityPermissions

	owner2 := f.createMember(t, owner, "owner2")
	f.assignRole(t, owner.AccessToken, owner2.membershipID, ownerRole)
	adminM := f.createMember(t, owner, "admin")
	f.assignRole(t, owner.AccessToken, adminM.membershipID, roleID(role.SystemRoles.Tenant.Administrator))
	mgr1 := f.createMember(t, owner, "mgr1")
	f.assignRole(t, owner.AccessToken, mgr1.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	// mgr1 passes the manager-route permission only through an override.
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+mgr1.membershipID+"/permission-overrides", owner.AccessToken,
		ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Users.Update, p.Users.UpdatePermissions}, Revoked: []string{}}); r.status != http.StatusNoContent {
		t.Fatalf("grant users.update override: %d %s", r.status, r.body)
	}
	mgr2 := f.createMember(t, owner, "mgr2")
	f.assignRole(t, owner.AccessToken, mgr2.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+exec.membershipID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: mgr2.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("link exec -> mgr2: %d %s", r.status, r.body)
	}
	newcomer := f.createMember(t, owner, "newcomer")
	adminTok := f.loginToken(t, adminM.email, adminM.password)
	mgr1Tok := f.loginToken(t, mgr1.email, mgr1.password)

	const insufficient = "insufficient_rank"
	userPath := func(m createdMember, suffix string) string { return "/api/v1/users/" + m.membershipID + suffix }

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("assign: an Administrator cannot assign Company Owner", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, userPath(newcomer, "/roles"), adminTok, ports.AssignUserRoleRequest{RoleID: ownerRole})
		f.wantForbidden(t, "admin assigns Company Owner", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, newcomer.membershipID); slices.Contains(u.RoleIDs, ownerRole) {
			t.Errorf("newcomer holds Company Owner after a refused assign: %v", u.RoleIDs)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("revoke: an Administrator cannot strip an owner's Company Owner role", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodDelete, userPath(owner2, "/roles/"+ownerRole), adminTok, nil)
		f.wantForbidden(t, "admin revokes Company Owner", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, owner2.membershipID); !slices.Contains(u.RoleIDs, ownerRole) {
			t.Errorf("owner2 lost Company Owner despite the refusal: %v", u.RoleIDs)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("overrides: an Administrator cannot change an owner's overrides", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPatch, userPath(owner2, "/permission-overrides"), adminTok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{p.Crm.Leads.Read}})
		f.wantForbidden(t, "admin revokes an owner's permission", r, http.StatusForbidden, insufficient)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivate: an Administrator cannot deactivate an owner", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, userPath(owner2, "/deactivate"), adminTok,
			ports.DeactivateUserRequest{Reason: "rank test"})
		f.wantForbidden(t, "admin deactivates an owner", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, owner2.membershipID); u.Status != "active" {
			t.Errorf("owner2 status = %q after a refused deactivate, want active", u.Status)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("reactivate: an Administrator cannot reactivate an owner", func(t *testing.T) {
		if r := f.authedJSON(t, http.MethodPost, userPath(owner2, "/deactivate"), owner.AccessToken,
			ports.DeactivateUserRequest{Reason: "setup for the reactivate rank test"}); r.status != http.StatusNoContent {
			t.Fatalf("owner deactivates owner2: %d %s", r.status, r.body)
		}
		r := f.authedJSON(t, http.MethodPost, userPath(owner2, "/reactivate"), adminTok, nil)
		f.wantForbidden(t, "admin reactivates an owner", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, owner2.membershipID); u.Status == "active" {
			t.Errorf("owner2 reactivated by an Administrator")
		}
		if r := f.authedJSON(t, http.MethodPost, userPath(owner2, "/reactivate"), owner.AccessToken, nil); r.status != http.StatusNoContent {
			t.Fatalf("owner reactivates owner2: %d %s", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("set manager: a Manager cannot appoint themselves over an Administrator", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPut, userPath(adminM, "/manager"), mgr1Tok,
			ports.AssignUserManagerRequest{ManagerID: mgr1.membershipID})
		f.wantForbidden(t, "manager becomes an admin's manager", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, adminM.membershipID); u.ReportsTo != "" {
			t.Errorf("admin reports_to = %q after a refused appointment", u.ReportsTo)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("clear manager: a Manager cannot take a report from an equal-rank manager", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodDelete, userPath(exec, "/manager"), mgr1Tok, nil)
		f.wantForbidden(t, "manager clears an equal-rank manager's link", r, http.StatusForbidden, insufficient)
		if u := f.getUser(t, owner.AccessToken, exec.membershipID); u.ReportsTo != mgr2.membershipID {
			t.Errorf("exec reports_to = %q after a refused clear, want %s", u.ReportsTo, mgr2.membershipID)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("overrides: an Administrator cannot strip tenant.admin from an owner", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPatch, userPath(owner2, "/permission-overrides"), adminTok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{p.Meta.TenantAdmin}})
		f.wantForbidden(t, "admin revokes tenant.admin from an owner", r, http.StatusForbidden, insufficient)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("overrides: a Manager cannot grant tenant.admin to anyone", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPatch, userPath(exec, "/permission-overrides"), mgr1Tok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Meta.TenantAdmin}, Revoked: []string{}})
		f.wantForbidden(t, "manager grants tenant.admin", r, http.StatusForbidden, "grant_exceeds_authority")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("set manager: a Manager cannot appoint themselves over a peer", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPut, userPath(mgr2, "/manager"), mgr1Tok,
			ports.AssignUserManagerRequest{ManagerID: mgr1.membershipID})
		f.wantForbidden(t, "manager becomes a peer's manager", r, http.StatusForbidden, insufficient)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("super user: the sole owner can be neither demoted nor deactivated, by anyone", func(t *testing.T) {
		sole := f.registerAndLogin(t, "sole")
		soleOwnerRole := f.roleIDByName(t, sole.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
		op := f.mintPlatformTokenWith(t, nil, true) // a super user is exempt from rank, never from the last-owner guard
		r := f.doWithHeaders(t, http.MethodDelete, "/api/v1/users/"+sole.MembershipID+"/roles/"+soleOwnerRole, op, nil, scopeHdr(sole.TenantID))
		f.wantForbidden(t, "operator revokes the sole owner's role", r, http.StatusConflict, "last_owner_protected")
		r = f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+sole.MembershipID+"/deactivate", op, ports.DeactivateUserRequest{Reason: "sole owner test"}, scopeHdr(sole.TenantID))
		f.wantForbidden(t, "operator deactivates the sole owner", r, http.StatusConflict, "last_owner_protected")
		if u := f.getUser(t, sole.AccessToken, sole.MembershipID); u.Status != "active" || !slices.Contains(u.RoleIDs, soleOwnerRole) {
			t.Errorf("sole owner changed by a refused call: %+v", u)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("an Administrator cannot strip the sole owner either (rank first)", func(t *testing.T) {
		sole := f.registerAndLogin(t, "sole2")
		a := f.createMember(t, sole, "admin")
		f.assignRole(t, sole.AccessToken, a.membershipID, f.roleIDByName(t, sole.AccessToken, role.SystemRoles.Tenant.Administrator))
		tok := f.loginToken(t, a.email, a.password)
		ownerR := f.roleIDByName(t, sole.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
		r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+sole.MembershipID+"/roles/"+ownerR, tok, nil)
		f.wantForbidden(t, "admin revokes the sole owner's role", r, http.StatusForbidden, insufficient)
		r = f.authedJSON(t, http.MethodPost, "/api/v1/users/"+sole.MembershipID+"/deactivate", tok, ports.DeactivateUserRequest{Reason: "rank test"})
		f.wantForbidden(t, "admin deactivates the sole owner", r, http.StatusForbidden, insufficient)
	})
}
