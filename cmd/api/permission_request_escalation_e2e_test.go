//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// The permission-elevation workflow must never be a path to authority a
// requester could not be given by role: tenant.admin and platform-reserved
// names are not requestable, and a platform-tenant member with no platform
// permission is not an approver. These tests walk the tenant-takeover chain
// through the real mux.

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

const elevationReason = "needed for the quarterly close"

func (f e2eFixture) assignRole(t *testing.T, token, membershipID, roleID string) {
	t.Helper()
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+membershipID+"/roles", token,
		ports.AssignUserRoleRequest{RoleID: roleID}); r.status != http.StatusNoContent {
		t.Fatalf("assign role %s to %s: %d %s", roleID, membershipID, r.status, r.body)
	}
}

func (f e2eFixture) requestElevation(t *testing.T, token, perm string) (httpResp, string) {
	t.Helper()
	r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", token,
		ports.CreatePermissionRequestRequest{Permission: perm, Reason: elevationReason})
	var out ports.CreatePermissionRequestResponse
	if r.status == http.StatusCreated {
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	}
	return r, out.RequestID
}

func (f e2eFixture) approveRequest(t *testing.T, token, requestID string) httpResp {
	t.Helper()
	return f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+requestID+"/approve", token,
		ports.ApprovePermissionRequestRequest{DecisionReason: "ok"})
}

// TestE2E_PermissionRequest_ManagerSelfAppointmentChainIsClosed walks the
// tenant-takeover chain: an HR Manager appoints themselves manager of a
// colluding executive (allowed: they outrank them), the colluder requests tenant.admin, the HR Manager
// approves, and the colluder assigns Company Owner to the HR Manager. Every
// link that still works is reported; the chain must die at the request.
func TestE2E_PermissionRequest_ManagerSelfAppointmentChainIsClosed(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "chain")
	hr := f.createMember(t, owner, "hr")
	colluder := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, hr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.HrManager))
	f.assignRole(t, owner.AccessToken, colluder.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	hrTok := f.loginToken(t, hr.email, hr.password)

	// Link 1: the HR Manager makes themselves the manager of a member they
	// outrank. That is ordinary team organisation and is allowed (the HR Manager
	// holds identity.users.update again, ADR 0069); the chain must die later, at
	// the request. The cases an HR Manager must NOT be able to do are in
	// TestE2E_HrManager_CannotAppointOverPeersOrTakeReportsFromEquals.
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+colluder.membershipID+"/manager", hrTok,
		ports.AssignUserManagerRequest{ManagerID: hr.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("link 1: HR Manager sets themselves manager of an executive: %d %s, want 204", r.status, r.body)
	}

	// Link 2: the colluder asks for tenant.admin.
	colluderTok := f.loginToken(t, colluder.email, colluder.password)
	sub, reqID := f.requestElevation(t, colluderTok, permission.IdentityPermissions.Meta.TenantAdmin)
	if sub.status != http.StatusUnprocessableEntity || decodeError(t, sub.body).Error != "permission_not_requestable" {
		t.Errorf("link 2: tenant.admin request: %d %s, want 422 permission_not_requestable", sub.status, sub.body)
	}
	if sub.status != http.StatusCreated {
		return
	}

	// Links 3 and 4 only run when link 2 let the request in.
	ap := f.approveRequest(t, hrTok, reqID)
	t.Logf("link 3: HR Manager approves tenant.admin for the colluder: %d", ap.status)
	if ap.status != http.StatusNoContent {
		return
	}
	colluderTok = f.loginToken(t, colluder.email, colluder.password)
	ownerRole := f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+hr.membershipID+"/roles", colluderTok,
		ports.AssignUserRoleRequest{RoleID: ownerRole}); r.status == http.StatusNoContent {
		t.Errorf("TAKEOVER COMPLETE: the HR Manager now holds Company Owner (links 2, 3 and 4 all succeeded)")
	}
}

// TestE2E_PermissionRequest_StoredNonRequestableRequestCannotBeApproved: a
// pending row written before the rule existed (tenant.admin) is refused on
// approve with 422 permission_not_requestable and stays pending.
func TestE2E_PermissionRequest_StoredNonRequestableRequestCannotBeApproved(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "stored")
	mgr := f.createMember(t, owner, "mgr")
	req := f.createMember(t, owner, "req")
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesManager))
	f.assignRole(t, owner.AccessToken, req.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+req.membershipID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: mgr.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager: %d %s", r.status, r.body)
	}
	mgrTok := f.loginToken(t, mgr.email, mgr.password)
	reqTok := f.loginToken(t, req.email, req.password)

	_, reqID := f.requestElevation(t, reqTok, permission.IdentityPermissions.Users.View)
	if reqID == "" {
		t.Fatal("seed request not created")
	}
	// Age the stored row into the old shape: the name becomes tenant.admin.
	identitytest.SetPermissionRequestPermission(t, f.Pool, reqID, permission.IdentityPermissions.Meta.TenantAdmin)

	ap := f.approveRequest(t, mgrTok, reqID)
	if ap.status != http.StatusUnprocessableEntity || decodeError(t, ap.body).Error != "permission_not_requestable" {
		t.Fatalf("approve stored tenant.admin request: %d %s, want 422 permission_not_requestable", ap.status, ap.body)
	}
	got := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+reqID, reqTok, nil)
	var dto ports.PermissionRequestDto
	if err := json.Unmarshal(got.body, &dto); err != nil || dto.State != "pending" {
		t.Errorf("request after refused approve: %d %s (err %v), want state pending", got.status, got.body, err)
	}
	// The approver can still close it by denying it.
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/deny", mgrTok,
		ports.DenyPermissionRequestRequest{DecisionReason: "not requestable"}); d.status != http.StatusNoContent {
		t.Errorf("deny stored request: %d %s", d.status, d.body)
	}
}

// TestE2E_PermissionRequest_OrdinaryPermissionStillFlows: a requestable
// permission goes request → manager approves → the requester holds it.
func TestE2E_PermissionRequest_OrdinaryPermissionStillFlows(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "flow")
	mgr := f.createMember(t, owner, "mgr")
	req := f.createMember(t, owner, "req")
	f.assignRole(t, owner.AccessToken, mgr.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesManager))
	f.assignRole(t, owner.AccessToken, req.membershipID, f.roleIDByName(t, owner.AccessToken, role.SystemRoles.Tenant.SalesExecutive))
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+req.membershipID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: mgr.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("set manager: %d %s", r.status, r.body)
	}
	mgrTok := f.loginToken(t, mgr.email, mgr.password)
	reqTok := f.loginToken(t, req.email, req.password)

	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", reqTok, nil); r.status != http.StatusForbidden {
		t.Fatalf("requester lists users before the grant: %d, want 403", r.status)
	}
	sub, reqID := f.requestElevation(t, reqTok, permission.IdentityPermissions.Users.View)
	if sub.status != http.StatusCreated {
		t.Fatalf("submit: %d %s", sub.status, sub.body)
	}
	if ap := f.approveRequest(t, mgrTok, reqID); ap.status != http.StatusNoContent {
		t.Fatalf("approve: %d %s", ap.status, ap.body)
	}
	// The approval rotated the requester's stamp (their old token is stale by design); log in again.
	f.evictStamp(t, req.personID)
	reqTok = f.loginToken(t, req.email, req.password)
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/users", reqTok, nil); r.status != http.StatusOK {
		t.Errorf("requester lists users after the grant: %d %s, want 200", r.status, r.body)
	}
}

// registerPlatformTenant onboards the tenant whose slug is "platform", so its
// members' tokens are slug-anchored platform principals.
func (f e2eFixture) registerPlatformTenant(t *testing.T) registeredTenant {
	t.Helper()
	slugVO, err := slug.New(tenant.PlatformSlug)
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, err := email.New("platform-owner@e2e.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	const password = "Correct-Horse-Battery-9!platform"
	out, err := f.app.Commands.RegisterTenant.Handle(t.Context(), command.RegisterTenantCommand{
		Slug: slugVO, LegalName: "Platform Pvt Ltd", DisplayName: "Platform",
		AdminEmail: addr, AdminPassword: password, AdminFirstName: "Platform", AdminLastName: "Owner",
	})
	if err != nil {
		t.Fatalf("register platform tenant: %v", err)
	}
	return registeredTenant{
		TenantID: string(out.TenantID), PersonID: string(out.PersonID), MembershipID: string(out.MembershipID),
		Slug: tenant.PlatformSlug, AccessToken: f.loginToken(t, addr.String(), password),
	}
}

// TestE2E_PermissionRequest_PlatformTenantMembersCannotSelfElevate: two
// platform-tenant members with no role must not be able to mint platform
// authority between them. Platform-tenant slug membership is not approver
// authority, and neither is platform.tenants.view: in the platform tenant an
// operator follows rank like anyone else (the operator exemption is
// cross-tenant only, ADR 0069 R3), so a read-only operator cannot approve.
func TestE2E_PermissionRequest_PlatformTenantMembersCannotSelfElevate(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	plat := f.registerPlatformTenant(t)
	a := f.createMember(t, plat, "plat-a")
	b := f.createMember(t, plat, "plat-b")
	mint := func(m createdMember, perms ...string) string {
		tok, err := f.Issuer.Issue(jwt.IssueArgs{
			PersonID: m.personID, TenantID: plat.TenantID, TenantSlug: tenant.PlatformSlug,
			MembershipID: m.membershipID, SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, m.personID),
			IsPlatform: true, Permissions: perms,
		})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return tok
	}
	elev := permission.IdentityPermissions.Meta.RequestPermissionElevation
	aTok := mint(a, elev)
	bTok := mint(b)

	sub, _ := f.requestElevation(t, aTok, permission.IdentityPermissions.Platform.ImpersonationManage)
	if sub.status != http.StatusUnprocessableEntity || decodeError(t, sub.body).Error != "permission_not_requestable" {
		t.Errorf("submit platform.impersonation.manage: %d %s, want 422 permission_not_requestable", sub.status, sub.body)
	}

	// A requestable permission, approved by a platform member who holds no
	// platform permission at all and is not the manager: refused.
	sub, reqID := f.requestElevation(t, aTok, permission.IdentityPermissions.Users.View)
	if sub.status != http.StatusCreated {
		t.Fatalf("submit users.view: %d %s", sub.status, sub.body)
	}
	if ap := f.approveRequest(t, bTok, reqID); ap.status != http.StatusNotFound {
		t.Errorf("zero-permission platform member approves: %d %s, want 404", ap.status, ap.body)
	}
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/deny", bTok,
		ports.DenyPermissionRequestRequest{DecisionReason: "not mine to decide"}); d.status != http.StatusNotFound {
		t.Errorf("zero-permission platform member denies: %d %s, want 404", d.status, d.body)
	}

	// A read-only operator (platform.tenants.view) in the platform tenant is
	// refused too: it used to approve (204); it now follows rank.
	opTok := mint(b, permission.IdentityPermissions.Platform.TenantsView)
	if ap := f.approveRequest(t, opTok, reqID); ap.status != http.StatusNotFound || decodeError(t, ap.body).Error != "permission_request_not_found" {
		t.Errorf("read-only operator approves in the platform tenant: %d %s, want 404 permission_request_not_found", ap.status, ap.body)
	}
}

// TestE2E_HrManager_CannotAppointOverPeersOrTakeReportsFromEquals: with
// identity.users.update back in the HR Manager seed, the manager commands are
// what keep the permission safe. An HR Manager cannot appoint themselves
// manager of someone they do not outrank (a peer manager, an Administrator),
// nor take a report from a manager of equal or higher rank, nor clear such a link.
func TestE2E_HrManager_CannotAppointOverPeersOrTakeReportsFromEquals(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "hrrank")
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	hr := f.createMember(t, owner, "hr")
	f.assignRole(t, owner.AccessToken, hr.membershipID, roleID(role.SystemRoles.Tenant.HrManager))
	peer := f.createMember(t, owner, "peer")
	f.assignRole(t, owner.AccessToken, peer.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	adminM := f.createMember(t, owner, "admin")
	f.assignRole(t, owner.AccessToken, adminM.membershipID, roleID(role.SystemRoles.Tenant.Administrator))
	senior := f.createMember(t, owner, "senior")
	f.assignRole(t, owner.AccessToken, senior.membershipID, roleID(role.SystemRoles.Tenant.SeniorManager))
	exec := f.createMember(t, owner, "exec")
	f.assignRole(t, owner.AccessToken, exec.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	hrTok := f.loginToken(t, hr.email, hr.password)
	put := func(sub, mgr string) httpResp {
		return f.authedJSON(t, http.MethodPut, "/api/v1/users/"+sub+"/manager", hrTok, ports.AssignUserManagerRequest{ManagerID: mgr})
	}
	for name, sub := range map[string]string{"a peer manager": peer.membershipID, "an Administrator": adminM.membershipID, "a Senior Manager": senior.membershipID} {
		r := put(sub, hr.membershipID)
		if r.status != http.StatusForbidden || decodeError(t, r.body).Error != "insufficient_rank" {
			t.Errorf("HR Manager appoints themselves manager of %s: %d %s, want 403 insufficient_rank", name, r.status, r.body)
		}
	}
	// an executive reporting to an equal-rank (peer) and to a more senior manager
	execPeer := f.createMember(t, owner, "exec-peer")
	f.assignRole(t, owner.AccessToken, execPeer.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	for sub, mgr := range map[string]string{exec.membershipID: peer.membershipID, execPeer.membershipID: senior.membershipID} {
		if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+sub+"/manager", owner.AccessToken, ports.AssignUserManagerRequest{ManagerID: mgr}); r.status != http.StatusNoContent {
			t.Fatalf("owner links %s: %d %s", sub, r.status, r.body)
		}
		if r := put(sub, hr.membershipID); r.status != http.StatusForbidden || decodeError(t, r.body).Error != "insufficient_rank" {
			t.Errorf("HR Manager takes a report from an equal or senior manager: %d %s, want 403 insufficient_rank", r.status, r.body)
		}
		if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+sub+"/manager", hrTok, nil); r.status != http.StatusForbidden || decodeError(t, r.body).Error != "insufficient_rank" {
			t.Errorf("HR Manager clears an equal or senior manager's link: %d %s, want 403 insufficient_rank", r.status, r.body)
		}
	}
	// the safe case still works: an unmanaged executive can be taken into the HR team
	free := f.createMember(t, owner, "exec-free")
	f.assignRole(t, owner.AccessToken, free.membershipID, roleID(role.SystemRoles.Tenant.SalesExecutive))
	if r := put(free.membershipID, hr.membershipID); r.status != http.StatusNoContent {
		t.Errorf("HR Manager takes an unmanaged executive: %d %s, want 204", r.status, r.body)
	}
}
