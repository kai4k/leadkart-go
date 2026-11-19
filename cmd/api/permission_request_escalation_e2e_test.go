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
// colluding executive, the colluder requests tenant.admin, the HR Manager
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

	// Link 1: the HR Manager makes themselves the colluder's manager. The
	// seeded HR Manager must not hold identity.users.update any more.
	armed := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+colluder.membershipID+"/manager", hrTok,
		ports.AssignUserManagerRequest{ManagerID: hr.membershipID})
	if armed.status != http.StatusForbidden {
		t.Errorf("link 1: HR Manager set a manager: %d %s, want 403", armed.status, armed.body)
	}
	// Whatever link 1 did, make the HR Manager the colluder's manager as the
	// owner would, so the remaining links are exercised on their own.
	if r := f.authedJSON(t, http.MethodPut, "/api/v1/users/"+colluder.membershipID+"/manager", owner.AccessToken,
		ports.AssignUserManagerRequest{ManagerID: hr.membershipID}); r.status != http.StatusNoContent {
		t.Fatalf("owner sets manager: %d %s", r.status, r.body)
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
// platform-tenant members with no permissions must not be able to mint
// platform authority between them. Platform-tenant slug membership is not
// approver authority; an operator holding platform.tenants.view still is.
func TestE2E_PermissionRequest_PlatformTenantMembersCannotSelfElevate(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	plat := f.registerPlatformTenant(t)
	a := f.createMember(t, plat, "plat-a")
	b := f.createMember(t, plat, "plat-b")
	aTok := f.loginToken(t, a.email, a.password)
	bTok := f.loginToken(t, b.email, b.password)

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
	if ap := f.approveRequest(t, bTok, reqID); ap.status != http.StatusForbidden {
		t.Errorf("zero-permission platform member approves: %d %s, want 403", ap.status, ap.body)
	}
	if d := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/deny", bTok,
		ports.DenyPermissionRequestRequest{DecisionReason: "not mine to decide"}); d.status != http.StatusForbidden {
		t.Errorf("zero-permission platform member denies: %d %s, want 403", d.status, d.body)
	}

	// An operator with platform.tenants.view (real membership in the platform
	// tenant) still approves.
	opTok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID: b.personID, TenantID: plat.TenantID, TenantSlug: tenant.PlatformSlug,
		MembershipID: b.membershipID, SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, b.personID),
		IsPlatform:  true,
		Permissions: []string{permission.IdentityPermissions.Platform.TenantsView},
	})
	if err != nil {
		t.Fatalf("mint operator: %v", err)
	}
	if ap := f.approveRequest(t, opTok, reqID); ap.status != http.StatusNoContent {
		t.Errorf("operator with platform.tenants.view approves: %d %s, want 204", ap.status, ap.body)
	}
}
