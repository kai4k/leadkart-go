//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// ADR 0069 A6: approving and denying a permission request is decided by the
// authority policy under the tenant lock, in the order existence -> authority ->
// state; who may SEE a request and which requests are in an approver's queue
// follow the same policy. One scenario per rule, through the real mux.

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

type prE2E struct {
	f      e2eFixture
	owner  registeredTenant
	tl     createdMember // Team Lead T
	tlTok  string
	mgr    createdMember // Sales Manager who is nobody's manager here
	mgrTok string
	admin  createdMember // Administrator who lost tenant.admin
	adTok  string
	peer   createdMember // another Team Lead
	peerTk string
	n      int
}

// exec creates a Sales Executive reporting to T and returns it with a token.
func (w *prE2E) exec(t *testing.T) (createdMember, string) {
	t.Helper()
	w.n++
	e := w.f.createMember(t, w.owner, "exec")
	w.f.assignRole(t, w.owner.AccessToken, e.membershipID, w.f.roleIDByName(t, w.owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	if r := w.f.authedJSON(t, http.MethodPut, "/api/v1/users/"+e.membershipID+"/manager", w.owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: w.tl.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager: %d %s", r.status, r.body)
	}
	return e, w.f.loginToken(t, e.email, e.password)
}

func (w *prE2E) decide(t *testing.T, verb, token, reqID string) httpResp {
	t.Helper()
	body := any(ports.ApprovePermissionRequestRequest{DecisionReason: "ok"})
	if verb == "deny" {
		body = ports.DenyPermissionRequestRequest{DecisionReason: "not now"}
	}
	return w.f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/"+verb, token, body)
}

func newPRE2E(t *testing.T) *prE2E {
	t.Helper()
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "prauth")
	w := &prE2E{f: f, owner: owner}
	role := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	mk := func(prefix, roleName string) createdMember {
		m := f.createMember(t, owner, prefix)
		f.assignRole(t, owner.AccessToken, m.membershipID, role(roleName))
		return m
	}
	w.tl = mk("tl", rolesTenant.TeamLead)
	w.peer = mk("peer", rolesTenant.TeamLead)
	w.mgr = mk("mgr", rolesTenant.SalesManager)
	w.admin = mk("admin", rolesTenant.Administrator)
	f.grantOverrides(t, owner.AccessToken, w.admin.membershipID, []string{}, []string{permission.IdentityPermissions.Meta.TenantAdmin})
	w.tlTok = f.loginToken(t, w.tl.email, w.tl.password)
	w.peerTk = f.loginToken(t, w.peer.email, w.peer.password)
	w.mgrTok = f.loginToken(t, w.mgr.email, w.mgr.password)
	w.adTok = f.loginToken(t, w.admin.email, w.admin.password)
	return w
}

var rolesTenant = role.SystemRoles.Tenant

func TestE2E_PermissionRequests_ApprovalDecidedByPolicy(t *testing.T) {
	w := newPRE2E(t)
	f := w.f
	p := permission.IdentityPermissions

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a manager approves a held permission for a report; the grant is exactly that permission", func(t *testing.T) {
		e, eTok := w.exec(t)
		sub, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		if sub.status != http.StatusCreated {
			t.Fatalf("submit: %d %s", sub.status, sub.body)
		}
		if r := w.decide(t, "approve", w.tlTok, id); r.status != http.StatusNoContent {
			t.Fatalf("approve: %d %s", r.status, r.body)
		}
		if u := f.getUser(t, w.owner.AccessToken, e.membershipID); u.Status != "active" {
			t.Errorf("requester status %q", u.Status)
		}
		got := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, eTok, nil)
		var dto ports.PermissionRequestDto
		if err := json.Unmarshal(got.body, &dto); err != nil || dto.State != "approved" || dto.ApproverMembershipID != w.tl.membershipID {
			t.Errorf("request after approval: %d %s", got.status, got.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("the chain: a manager cannot approve a permission they do not hold", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Roles.Update)
		r := w.decide(t, "approve", w.tlTok, id)
		f.wantForbidden(t, "Team Lead approves identity.roles.update", r, http.StatusForbidden, "grant_exceeds_authority")
		got := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, eTok, nil)
		var dto ports.PermissionRequestDto
		if err := json.Unmarshal(got.body, &dto); err != nil || dto.State != "pending" {
			t.Errorf("request after the refused approval: %d %s, want pending", got.status, got.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a manager who is not the requester's manager is not an approver", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		r := w.decide(t, "approve", w.mgrTok, id)
		// the Sales Manager outranks the executive, but is not their manager
		f.wantForbidden(t, "unrelated manager approves", r, http.StatusNotFound, "permission_request_not_found")
		r = w.decide(t, "deny", w.mgrTok, id)
		f.wantForbidden(t, "unrelated manager denies", r, http.StatusNotFound, "permission_request_not_found")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a peer cannot approve a peer's request", func(t *testing.T) {
		_, id := f.requestElevation(t, w.tlTok, p.Tasks.WorkItems.Reassign)
		r := w.decide(t, "approve", w.peerTk, id)
		f.wantForbidden(t, "peer approves", r, http.StatusNotFound, "permission_request_not_found")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("self approval is refused", func(t *testing.T) {
		_, id := f.requestElevation(t, w.tlTok, p.Crm.Leads.ReadAll)
		r := w.decide(t, "approve", w.tlTok, id)
		f.wantForbidden(t, "self approval", r, http.StatusForbidden, "self_modification_forbidden")
		r = w.decide(t, "deny", w.tlTok, id)
		f.wantForbidden(t, "self denial", r, http.StatusForbidden, "self_modification_forbidden")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("an Administrator without tenant.admin approves day-to-day work but not the must-hold set", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, day := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		if r := w.decide(t, "approve", w.adTok, day); r.status != http.StatusNoContent {
			t.Errorf("admin approves a day-to-day permission for a user: %d %s", r.status, r.body)
		}
		_, must := f.requestElevation(t, eTok, p.Roles.Update)
		r := w.decide(t, "approve", w.adTok, must)
		f.wantForbidden(t, "admin approves identity.roles.update it does not hold", r, http.StatusForbidden, "grant_exceeds_authority")
		// denying needs eligibility only
		if r := w.decide(t, "deny", w.adTok, must); r.status != http.StatusNoContent {
			t.Errorf("admin denies a must-hold request: %d %s", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("an impersonation session cannot decide or submit", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		// A real session (the request gate keeps an impersonation token alive only while its session is).
		opTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
		sess := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", opTok,
			ports.CreateImpersonationSessionRequest{TargetTenantID: w.owner.TenantID, Reason: "support ticket 4711", DurationMinutes: 30})
		if sess.status != http.StatusCreated {
			t.Fatalf("create session: %d %s", sess.status, sess.body)
		}
		var created ports.CreateImpersonationSessionResponse
		if err := json.Unmarshal(sess.body, &created); err != nil {
			t.Fatalf("decode session: %v", err)
		}
		imp := created.AccessToken
		f.wantForbidden(t, "approve while impersonating", w.decide(t, "approve", imp, id), http.StatusForbidden, "impersonation_not_allowed")
		f.wantForbidden(t, "deny while impersonating", w.decide(t, "deny", imp, id), http.StatusForbidden, "impersonation_not_allowed")
		sub, _ := f.requestElevation(t, imp, p.Crm.Leads.ReadAll)
		f.wantForbidden(t, "submit while impersonating", sub, http.StatusForbidden, "impersonation_not_allowed")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("authority is decided before state: a stranger learns nothing about a decided request", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		if r := w.decide(t, "deny", w.tlTok, id); r.status != http.StatusNoContent {
			t.Fatalf("deny: %d %s", r.status, r.body)
		}
		f.wantForbidden(t, "eligible approver, decided request", w.decide(t, "approve", w.tlTok, id), http.StatusConflict, "permission_request_not_pending")
		f.wantForbidden(t, "stranger, decided request", w.decide(t, "approve", w.peerTk, id), http.StatusNotFound, "permission_request_not_found")
		missing := w.decide(t, "approve", w.tlTok, "01a1f625-2aff-792d-ba5e-4ecb47aeda77")
		f.wantForbidden(t, "unknown request", missing, http.StatusNotFound, "permission_request_not_found")
		f.wantForbidden(t, "stranger, unknown request", w.decide(t, "approve", w.peerTk, "01a1f625-2aff-792d-ba5e-4ecb47aeda77"), http.StatusNotFound, "permission_request_not_found")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a requester deactivated meanwhile is a 409 and the request stays pending", func(t *testing.T) {
		e, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+e.membershipID+"/deactivate", w.owner.AccessToken, ports.DeactivateUserRequest{Reason: "left the company"}); r.status != http.StatusNoContent {
			t.Fatalf("deactivate: %d %s", r.status, r.body)
		}
		f.wantForbidden(t, "approve for a deactivated requester", w.decide(t, "approve", w.tlTok, id), http.StatusConflict, "permission_request_requester_inactive")
		if r := w.decide(t, "deny", w.tlTok, id); r.status != http.StatusNoContent {
			t.Errorf("the manager can still deny it: %d %s", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("visibility: the requester and anyone who may deny it; everyone else 404", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		for name, tok := range map[string]string{"requester": eTok, "their manager": w.tlTok, "an Administrator": w.adTok, "the owner": w.owner.AccessToken} {
			if r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, tok, nil); r.status != http.StatusOK {
				t.Errorf("%s reads the request: %d %s, want 200", name, r.status, r.body)
			}
		}
		for name, tok := range map[string]string{"a peer Team Lead": w.peerTk, "an unrelated manager": w.mgrTok} {
			r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, tok, nil)
			f.wantForbidden(t, name+" reads the request", r, http.StatusNotFound, "permission_request_not_found")
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("the approver queue lists only what the caller may decide", func(t *testing.T) {
		_, eTok := w.exec(t)
		_, id := f.requestElevation(t, eTok, p.Tasks.WorkItems.Reassign)
		queue := func(tok string) []string {
			r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests?role=approver&page_size=100", tok, nil)
			var out ports.ListPermissionRequestsResponse
			if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
				t.Fatalf("queue: %d %s", r.status, r.body)
			}
			ids := make([]string, 0, len(out.Requests))
			for _, q := range out.Requests {
				ids = append(ids, q.ID)
			}
			return ids
		}
		contains := func(list []string, id string) bool {
			for _, x := range list {
				if x == id {
					return true
				}
			}
			return false
		}
		if !contains(queue(w.tlTok), id) {
			t.Error("the manager's queue misses their report's pending request")
		}
		if !contains(queue(w.adTok), id) {
			t.Error("the Administrator's queue misses a request they may decide")
		}
		for name, tok := range map[string]string{"peer Team Lead": w.peerTk, "unrelated manager": w.mgrTok, "requester": eTok} {
			if contains(queue(tok), id) {
				t.Errorf("the %s's queue lists a request they would get 403 on", name)
			}
		}
		// everything in a queue is decidable: denying each listed request never answers 403
		if r := w.decide(t, "deny", w.mgrTok, id); r.status != http.StatusNotFound {
			t.Errorf("the unrelated manager denies it anyway: %d %s, want 404", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("submitting needs identity.meta.request_permission_elevation", func(t *testing.T) {
		_, eTok := w.exec(t)
		f.grantOverrides(t, w.owner.AccessToken, w.tl.membershipID, []string{}, []string{p.Meta.RequestPermissionElevation})
		f.evictStamp(t, w.tl.personID) // the override change rotated the stamp; log in again
		tok := f.loginToken(t, w.tl.email, w.tl.password)
		sub, _ := f.requestElevation(t, tok, p.Crm.Leads.ReadAll)
		f.wantForbidden(t, "submit without the elevation permission", sub, http.StatusForbidden, "forbidden")
		if sub, _ := f.requestElevation(t, eTok, p.Crm.Leads.ReadAll); sub.status != http.StatusCreated {
			t.Errorf("a member holding it submits: %d %s", sub.status, sub.body)
		}
		// the owner (tenant.admin implies the permission) may submit as well
		if sub, _ := f.requestElevation(t, w.owner.AccessToken, p.Crm.Leads.ReadAll); sub.status != http.StatusCreated {
			t.Errorf("an owner submits: %d %s", sub.status, sub.body)
		}
	})
}

// TestE2E_PermissionRequests_PlatformTenantFollowsRank: in the platform tenant a
// read-only operator (platform.tenants.view) has no approval authority; the
// operator exemption is cross-tenant only.
func TestE2E_PermissionRequests_PlatformTenantFollowsRank(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	plat := f.registerPlatformTenant(t)
	a := f.createMember(t, plat, "plat-a")
	b := f.createMember(t, plat, "plat-b")
	mint := func(m createdMember, perms ...string) string {
		tok, err := f.Issuer.Issue(jwt.IssueArgs{
			PersonID: m.personID, TenantID: plat.TenantID, TenantSlug: tenant.PlatformSlug, MembershipID: m.membershipID,
			SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, m.personID), IsPlatform: true, Permissions: perms,
		})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return tok
	}
	aTok := mint(a, permission.IdentityPermissions.Meta.RequestPermissionElevation)
	viewOnly := mint(b, permission.IdentityPermissions.Platform.TenantsView)
	_, id := f.requestElevation(t, aTok, permission.IdentityPermissions.Users.View)
	if id == "" {
		t.Fatal("platform member could not submit")
	}
	f.wantForbidden(t, "read-only operator approves", f.approveRequest(t, viewOnly, id), http.StatusNotFound, "permission_request_not_found")
	f.wantForbidden(t, "read-only operator denies", f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+id+"/deny", viewOnly,
		ports.DenyPermissionRequestRequest{DecisionReason: "not mine"}), http.StatusNotFound, "permission_request_not_found")
	// the platform tenant's owner (rank 0) decides
	if r := f.approveRequest(t, plat.AccessToken, id); r.status != http.StatusNoContent {
		t.Errorf("platform owner approves: %d %s", r.status, r.body)
	}
}

// Approving a name that is on the requester's REVOKED list is an un-revoke. It
// needs the authority to change their overrides on top of the approval rule, so
// an Administrator who does not hold the name cannot quietly reverse a
// deliberate revocation; an owner (who holds everything) can.
func TestE2E_PermissionRequests_ApprovingARevokedNameIsAnUnrevoke(t *testing.T) {
	w := newPRE2E(t)
	f := w.f
	p := permission.IdentityPermissions
	// day-to-day names: an Administrator may approve them without holding them. The
	// Team Lead (the requester's manager) holds Reassign but not Assign.
	assign, reassign := p.Crm.Leads.Assign, p.Tasks.WorkItems.Reassign
	holds := func(e createdMember, name string) bool {
		f.evictStamp(t, e.personID) // the overrides change rotated the stamp
		claims := tokenClaims(t, f.loginToken(t, e.email, e.password))
		perms, _ := claims["permission"].([]any)
		for _, x := range perms {
			if x == name {
				return true
			}
		}
		return false
	}
	setup := func(t *testing.T, revoked string) (createdMember, string, string) {
		t.Helper()
		e, _ := w.exec(t)
		f.grantOverrides(t, w.owner.AccessToken, e.membershipID, []string{}, []string{revoked})
		f.evictStamp(t, e.personID)
		eTok := f.loginToken(t, e.email, e.password)
		sub, id := f.requestElevation(t, eTok, revoked)
		if sub.status != http.StatusCreated {
			t.Fatalf("submit: %d %s", sub.status, sub.body)
		}
		return e, eTok, id
	}
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("an Administrator who does not hold the name cannot un-revoke it", func(t *testing.T) {
		e, eTok, id := setup(t, assign)
		r := w.decide(t, "approve", w.adTok, id)
		f.wantForbidden(t, "admin approves a name an admin revoked", r, http.StatusForbidden, "grant_exceeds_authority")
		got := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, eTok, nil)
		var dto ports.PermissionRequestDto
		if err := json.Unmarshal(got.body, &dto); err != nil || dto.State != "pending" {
			t.Errorf("the refused approval left the request: %d %s, want pending", got.status, got.body)
		}
		if holds(e, assign) {
			t.Error("the refused approval un-revoked the permission")
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("the owner can: the revoke is gone", func(t *testing.T) {
		e, _, id := setup(t, assign)
		if r := w.decide(t, "approve", w.owner.AccessToken, id); r.status != http.StatusNoContent {
			t.Fatalf("owner approves the un-revoke: %d %s, want 204", r.status, r.body)
		}
		if !holds(e, assign) {
			t.Error("the owner's approval did not restore the permission")
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a manager who holds the name cannot un-revoke it (an explicit revoke needs the Admin tier)", func(t *testing.T) {
		e, eTok, id := setup(t, reassign)
		r := w.decide(t, "approve", w.tlTok, id)
		f.wantForbidden(t, "the requester's manager (who holds "+reassign+") approves", r, http.StatusForbidden, "grant_exceeds_authority")
		got := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, eTok, nil)
		var dto ports.PermissionRequestDto
		if err := json.Unmarshal(got.body, &dto); err != nil || dto.State != "pending" {
			t.Errorf("the refused approval left the request: %d %s, want pending", got.status, got.body)
		}
		if holds(e, reassign) {
			t.Error("the refused approval un-revoked the permission")
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("the overrides route has the same rule: a manager cannot lift a revoke either", func(t *testing.T) {
		e, _ := w.exec(t)
		f.grantOverrides(t, w.owner.AccessToken, e.membershipID, []string{}, []string{reassign})
		f.grantOverrides(t, w.owner.AccessToken, w.mgr.membershipID, []string{p.Users.UpdatePermissions, reassign}, nil)
		f.evictStamp(t, w.mgr.personID)
		mTok := f.loginToken(t, w.mgr.email, w.mgr.password)
		r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+e.membershipID+"/permission-overrides", mTok,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}})
		f.wantForbidden(t, "manager lifts a revoke", r, http.StatusForbidden, "grant_exceeds_authority")
		// The Administrator tier and above may.
		if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+e.membershipID+"/permission-overrides", w.owner.AccessToken,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}); r.status != http.StatusNoContent {
			t.Errorf("owner lifts the revoke: %d %s, want 204", r.status, r.body)
		}
	})
}
