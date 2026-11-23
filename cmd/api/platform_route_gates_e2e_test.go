//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Platform routes are gated on their PERMISSION, not on platform-tenant
// membership alone (ADR 0068). A platform-tenant member holding no permission
// must be refused on every mutating and cross-tenant platform route, with or
// without X-Tenant-Id, and the victim must be untouched.

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

type platformRoute struct {
	name   string
	method string
	path   func(v registeredTenant) string
	body   func(v registeredTenant) any
}

func platformGateRoutes() []platformRoute {
	reason := func(registeredTenant) any { return ports.SuspendTenantRequest{Reason: "gate test reason"} }
	none := func(registeredTenant) any { return nil }
	return []platformRoute{
		{"suspend tenant", http.MethodPost, func(v registeredTenant) string { return "/api/v1/tenants/" + v.TenantID + "/suspend" }, reason},
		{"activate tenant", http.MethodPost, func(v registeredTenant) string { return "/api/v1/tenants/" + v.TenantID + "/activate" }, none},
		{"mark tenant for deletion", http.MethodPost, func(v registeredTenant) string { return "/api/v1/tenants/" + v.TenantID + "/mark-for-deletion" }, reason},
		{"restore tenant", http.MethodPost, func(v registeredTenant) string { return "/api/v1/tenants/" + v.TenantID + "/restore" }, none},
		{"hard delete tenant", http.MethodDelete, func(v registeredTenant) string { return "/api/v1/platform/tenants/" + v.TenantID }, none},
		{"global suspend person", http.MethodPost, func(v registeredTenant) string { return "/api/v1/platform/persons/" + v.PersonID + "/global-suspend" },
			func(registeredTenant) any { return ports.GlobalSuspendPersonRequest{Reason: "gate test reason"} }},
		{"lift global suspension", http.MethodPost, func(v registeredTenant) string {
			return "/api/v1/platform/persons/" + v.PersonID + "/lift-global-suspension"
		}, none},
		{"update person profile", http.MethodPatch, func(v registeredTenant) string { return "/api/v1/platform/persons/" + v.PersonID + "/profile" },
			func(registeredTenant) any {
				return ports.UpdatePersonProfileRequest{FirstName: "Hijacked", LastName: "Name"}
			}},
		{"create impersonation session", http.MethodPost, func(registeredTenant) string { return "/api/v1/platform/impersonation/sessions" },
			func(v registeredTenant) any {
				return ports.CreateImpersonationSessionRequest{TargetTenantID: v.TenantID, Reason: "ticket #1 gate test", DurationMinutes: 30}
			}},
		{"list impersonation sessions", http.MethodGet, func(registeredTenant) string { return "/api/v1/platform/impersonation/sessions" }, none},
		{"list all tenants", http.MethodGet, func(registeredTenant) string { return "/api/v1/platform/tenants" }, none},
		{"get person", http.MethodGet, func(v registeredTenant) string { return "/api/v1/platform/persons/" + v.PersonID }, none},
		{"get person by email", http.MethodGet, func(v registeredTenant) string { return "/api/v1/platform/persons?email=" + url.QueryEscape(v.Email) }, none},
		{"list person memberships", http.MethodGet, func(v registeredTenant) string { return "/api/v1/platform/persons/" + v.PersonID + "/memberships" }, none},
		{"platform stats", http.MethodGet, func(registeredTenant) string { return "/api/v1/platform/stats" }, none},
		{"omni search", http.MethodGet, func(registeredTenant) string { return "/api/v1/search?q=gate" }, none},
	}
}

// assertVictimIntact proves the victim's person, membership and tenant are
// exactly as registered: the token still verifies (global suspend
// rotates the stamp), the email is unchanged and the tenant is active.
func (f e2eFixture) assertVictimIntact(t *testing.T, what string, v registeredTenant) {
	t.Helper()
	u := f.getUser(t, v.AccessToken, v.MembershipID)
	if u.Email != v.Email || u.Status != "active" {
		t.Errorf("%s: victim user changed: email=%q status=%q", what, u.Email, u.Status)
	}
	if got := f.tenantStatus(t, v); got != victimTenantStatus {
		t.Errorf("%s: victim tenant status %q, want %q", what, got, victimTenantStatus)
	}
}

// victimTenantStatus is the status of a freshly registered tenant.
const victimTenantStatus = "active" // owner ruling 2026-11-21: registration creates active companies

func (f e2eFixture) tenantStatus(t *testing.T, v registeredTenant) string {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+v.TenantID, v.AccessToken, nil)
	var td ports.TenantDto
	if err := json.Unmarshal(r.body, &td); err != nil || r.status != http.StatusOK {
		t.Fatalf("read victim tenant: %d %s", r.status, r.body)
	}
	return td.Status
}

// A platform-tenant member with ZERO permissions is refused on every mutating
// and cross-tenant platform route, with and without X-Tenant-Id; nothing changes.
func TestE2E_PlatformGates_ZeroPermissionMemberRefused(t *testing.T) {
	f := newE2EFixture(t)
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	for _, rt := range platformGateRoutes() {
		for _, withHeader := range []bool{false, true} {
			v := f.registerAndLogin(t, "gatevictim")
			var hdr map[string]string
			label := rt.name + " no header"
			if withHeader {
				hdr = scopeHdr(v.TenantID)
				label = rt.name + " with X-Tenant-Id"
			}
			before := f.getUser(t, v.AccessToken, v.MembershipID)
			r := f.doWithHeaders(t, rt.method, rt.path(v), zero, rt.body(v), hdr)
			if r.status != http.StatusForbidden {
				t.Errorf("%s: zero-permission platform member got %d %s, want 403", label, r.status, r.body)
			}
			f.assertVictimIntact(t, label, v)
			f.assertPersonNamesUnchanged(t, label, v, before)
		}
	}
	// None of the attempts above may have opened an impersonation session.
	lister := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{permission.IdentityPermissions.Platform.ImpersonationManage}, false)
	lr := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", lister, nil)
	var sessions ports.ListImpersonationSessionsResponse
	if err := json.Unmarshal(lr.body, &sessions); err != nil || lr.status != http.StatusOK {
		t.Fatalf("list impersonation sessions: %d %s", lr.status, lr.body)
	}
	if len(sessions.Sessions) != 0 {
		t.Errorf("%d impersonation session(s) exist after zero-permission attempts, want none: %s", len(sessions.Sessions), lr.body)
	}
}

// assertPersonNamesUnchanged proves the victim person's names are what they
// were before the attempt (the platform profile-update route renames).
func (f e2eFixture) assertPersonNamesUnchanged(t *testing.T, what string, v registeredTenant, before ports.UserDto) {
	t.Helper()
	u := f.getUser(t, v.AccessToken, v.MembershipID)
	if u.FirstName != before.FirstName || u.LastName != before.LastName {
		t.Errorf("%s: person names changed to %q %q, want %q %q", what, u.FirstName, u.LastName, before.FirstName, before.LastName)
	}
}

// The reported Critical, in its minimal form: deactivate another tenant's user
// through the header as a platform member with no permissions.
func TestE2E_PlatformGates_DeactivateViaHeaderRefusedAndUniform(t *testing.T) {
	f := newE2EFixture(t)
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	v := f.registerAndLogin(t, "deactvictim")
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+v.MembershipID+"/deactivate", zero, ports.DeactivateUserRequest{Reason: "gate test reason"}, scopeHdr(v.TenantID))
	if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "tenant_scope_forbidden" {
		t.Fatalf("zero-permission member deactivate via header: %d %s, want 403 tenant_scope_forbidden", r.status, r.body)
	}
	f.assertVictimIntact(t, "deactivate via header", v)
}

// A zero-permission member sending a foreign tenant in the header gets the SAME
// 403 whether the tenant exists, is unknown or is suspended: no enumeration oracle.
func TestE2E_PlatformGates_ScopeForbiddenIsUniform(t *testing.T) {
	f := newE2EFixture(t)
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	active := f.registerAndLogin(t, "unifactive")
	suspended := f.registerAndLogin(t, "unifsusp")
	su := f.mintPlatformTokenWith(t, nil, true)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+suspended.TenantID+"/suspend", su, ports.SuspendTenantRequest{Reason: "uniformity test"}); r.status >= 300 {
		t.Fatalf("suspend fixture tenant: %d %s", r.status, r.body)
	}
	var want string
	for name, id := range map[string]string{"existing": active.TenantID, "unknown": unknownTenantID, "suspended": suspended.TenantID} {
		r := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", zero, nil, scopeHdr(id))
		if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "tenant_scope_forbidden" {
			t.Errorf("%s tenant: %d %s, want 403 tenant_scope_forbidden", name, r.status, r.body)
		}
		if want == "" {
			want = string(r.body)
		} else if string(r.body) != want {
			t.Errorf("%s tenant: body differs from the other cases (enumeration oracle): %s vs %s", name, r.body, want)
		}
	}
}

// An operator holding exactly the right permission succeeds; the same holder
// of a DIFFERENT platform permission does not.
func TestE2E_PlatformGates_RightPermissionSucceeds(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	scope := p.Platform.TenantsView

	deact := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{scope, p.Platform.UsersManage}, false)
	v := f.registerAndLogin(t, "okdeact")
	f.addSecondOwner(t, v, "okdeact-b") // the last owner cannot be deactivated (ADR 0069 H6)
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+v.MembershipID+"/deactivate", deact, ports.DeactivateUserRequest{Reason: "gate test reason"}, scopeHdr(v.TenantID)); r.status != http.StatusNoContent {
		t.Errorf("operator with platform.users.manage: %d %s, want 204", r.status, r.body)
	}

	susp := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Tenants.Suspend, p.Tenants.Activate}, false)
	w := f.registerAndLogin(t, "oksusp")
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+w.TenantID+"/suspend", susp, ports.SuspendTenantRequest{Reason: "gate test reason"}); r.status != http.StatusNoContent {
		t.Errorf("operator with identity.tenants.suspend: %d %s, want 204", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+w.TenantID+"/activate", susp, nil); r.status != http.StatusNoContent {
		t.Errorf("operator with identity.tenants.activate: %d %s, want 204", r.status, r.body)
	}
	// suspend permission does not open mark-for-deletion (identity.tenants.delete).
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+w.TenantID+"/mark-for-deletion", susp, ports.MarkTenantForDeletionRequest{Reason: "gate test reason"}); r.status != http.StatusForbidden {
		t.Errorf("suspend-only operator mark-for-deletion: %d %s, want 403", r.status, r.body)
	}

	users := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.UsersManage}, false)
	x := f.registerAndLogin(t, "okperson")
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/platform/persons/"+x.PersonID+"/profile", users, ports.UpdatePersonProfileRequest{FirstName: "Renamed", LastName: "Person"}); r.status >= 300 {
		t.Errorf("operator with platform.users.manage updating person profile: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", users, ports.CreateImpersonationSessionRequest{TargetTenantID: x.TenantID, Reason: "ticket #2 gate test", DurationMinutes: 30}); r.status != http.StatusForbidden {
		t.Errorf("platform.users.manage must not open impersonation: %d %s", r.status, r.body)
	}
	// Being allowed to edit tenants is not being allowed to become their admin.
	tenants := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.TenantsManage}, false)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", tenants, ports.CreateImpersonationSessionRequest{TargetTenantID: x.TenantID, Reason: "ticket #3 gate test", DurationMinutes: 30}); r.status != http.StatusForbidden {
		t.Errorf("platform.tenants.manage without platform.impersonation.manage: impersonation create %d %s, want 403", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/impersonation/sessions", tenants, nil); r.status != http.StatusForbidden {
		t.Errorf("platform.tenants.manage without platform.impersonation.manage: impersonation list %d %s, want 403", r.status, r.body)
	}
	imp := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage}, false)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", imp, ports.CreateImpersonationSessionRequest{TargetTenantID: x.TenantID, Reason: "ticket #4 gate test", DurationMinutes: 30}); r.status != http.StatusCreated {
		t.Errorf("platform.impersonation.manage impersonation create: %d %s, want 201", r.status, r.body)
	}
}

// tenant.admin of an ordinary tenant is not a platform actor: every platform
// route stays 403 (tenant.admin never implies platform-reserved names).
func TestE2E_PlatformGates_TenantAdminRefused(t *testing.T) {
	f := newE2EFixture(t)
	admin := f.registerAndLogin(t, "gateadmin")
	victim := f.registerAndLogin(t, "gatevict2")
	for _, rt := range platformGateRoutes() {
		r := f.authedJSON(t, rt.method, rt.path(victim), admin.AccessToken, rt.body(victim))
		if r.status != http.StatusForbidden {
			t.Errorf("%s as ordinary tenant.admin: %d %s, want 403", rt.name, r.status, r.body)
		}
	}
	f.assertVictimIntact(t, "tenant.admin sweep", victim)
}
