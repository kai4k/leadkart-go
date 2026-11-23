//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Path-tenant routes decide by their own gate: an X-Tenant-Id equal to the URL
// tenant changes nothing (the BFF always sends it while a tenant is selected),
// a different one is a 400 conflict, and no answer depends on whether the
// tenant exists or is suspended.

package main

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// An operator holding only the route's permission (no platform.tenants.view)
// gets the same answer with the matching header as without it.
func TestE2E_PathTenantRoutes_MatchingHeaderEqualsNoHeader(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	v := f.registerAndLogin(t, "pathhdr")

	manage := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsManage}, false)
	profile := ports.UpdateTenantProfileRequest{LegalName: "Header Path Pvt Ltd", DisplayName: "Header Path"}
	for name, hdr := range map[string]map[string]string{"without header": nil, "with matching header": scopeHdr(v.TenantID)} {
		if r := f.doWithHeaders(t, http.MethodPatch, "/api/v1/tenants/"+v.TenantID+"/profile", manage, profile, hdr); r.status != http.StatusNoContent {
			t.Errorf("platform.tenants.manage PATCH profile %s: %d %s, want 204", name, r.status, r.body)
		}
	}

	susp := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Tenants.Suspend, p.Tenants.Activate}, false)
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants/"+v.TenantID+"/suspend", susp, ports.SuspendTenantRequest{Reason: "path header test"}, scopeHdr(v.TenantID)); r.status != http.StatusNoContent {
		t.Errorf("identity.tenants.suspend POST suspend with matching header: %d %s, want 204", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants/"+v.TenantID+"/activate", susp, nil, scopeHdr(v.TenantID)); r.status != http.StatusNoContent {
		t.Errorf("identity.tenants.activate POST activate with matching header: %d %s, want 204", r.status, r.body)
	}
}

// A header naming a different tenant than the URL stays a 400 conflict.
func TestE2E_PathTenantRoutes_ConflictingHeaderIs400(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	a := f.registerAndLogin(t, "pathcfa")
	b := f.registerAndLogin(t, "pathcfb")
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsManage, p.Tenants.Suspend}, false)
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, "/api/v1/tenants/" + a.TenantID + "/profile", ports.UpdateTenantProfileRequest{LegalName: "X Pvt Ltd", DisplayName: "X"}},
		{http.MethodPost, "/api/v1/tenants/" + a.TenantID + "/suspend", ports.SuspendTenantRequest{Reason: "conflict test"}},
	} {
		r := f.doWithHeaders(t, rt.method, rt.path, op, rt.body, scopeHdr(b.TenantID))
		if r.status != http.StatusBadRequest || decodeErrorCode(t, r.body) != "tenant_scope_conflict" {
			t.Errorf("%s %s with another tenant's header: %d %s, want 400 tenant_scope_conflict", rt.method, rt.path, r.status, r.body)
		}
	}
}

// No enumeration oracle: a zero-permission platform member gets the route
// gate's 403 for an existing, an unknown and a suspended tenant alike, with the
// matching header and without it.
func TestE2E_PathTenantRoutes_ZeroPermissionAnswerIsUniform(t *testing.T) {
	f := newE2EFixture(t)
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	existing := f.registerAndLogin(t, "pathuni1")
	suspended := f.registerAndLogin(t, "pathuni2")
	su := f.mintPlatformTokenWith(t, nil, true)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+suspended.TenantID+"/suspend", su, ports.SuspendTenantRequest{Reason: "uniformity test"}); r.status >= 300 {
		t.Fatalf("suspend fixture tenant: %d %s", r.status, r.body)
	}
	routes := []struct {
		method, suffix string
		body           any
	}{
		{http.MethodPatch, "/api/v1/tenants/%s/profile", ports.UpdateTenantProfileRequest{LegalName: "Y Pvt Ltd", DisplayName: "Y"}},
		{http.MethodPost, "/api/v1/tenants/%s/suspend", ports.SuspendTenantRequest{Reason: "uniformity test"}},
	}
	for _, rt := range routes {
		var want string
		for name, id := range map[string]string{"existing": existing.TenantID, "unknown": unknownTenantID, "suspended": suspended.TenantID} {
			for hname, hdr := range map[string]map[string]string{"no header": nil, "matching header": scopeHdr(id)} {
				path := fmt.Sprintf(rt.suffix, id)
				r := f.doWithHeaders(t, rt.method, path, zero, rt.body, hdr)
				if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "forbidden" {
					t.Errorf("%s %s (%s, %s): %d %s, want the route gate's 403 forbidden", rt.method, path, name, hname, r.status, r.body)
					continue
				}
				if want == "" {
					want = string(r.body)
				} else if string(r.body) != want {
					t.Errorf("%s (%s, %s): body differs (enumeration oracle): %s vs %s", rt.method+" "+rt.suffix, name, hname, r.body, want)
				}
			}
		}
	}
}
