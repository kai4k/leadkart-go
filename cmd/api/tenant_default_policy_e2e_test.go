//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context(); no goroutines.
//
// arch-test:no-synctest — HTTP round trips and Postgres; no time-dependent logic.

// A company that never saved its security settings has the DEFAULT password
// policy, on every read that reports one, and a client that reads it can save it
// back. The OpenAPI PasswordPolicyDto declares min_length 8..128,
// max_failed_attempts 3..50 and lockout_minutes 0..1440; the web client's generated
// schema refuses anything else, so a zero here took a whole list down.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// requireInContractPolicy asserts p satisfies every constraint of the spec's
// PasswordPolicyDto.
func requireInContractPolicy(t *testing.T, what string, p ports.PasswordPolicyDto) {
	t.Helper()
	if p.MinLength < 8 || p.MinLength > 128 || p.MaxFailedAttempts < 3 || p.MaxFailedAttempts > 50 ||
		p.LockoutMinutes < 0 || p.LockoutMinutes > 1440 {
		t.Errorf("%s: password_policy %+v is outside the OpenAPI PasswordPolicyDto bounds", what, p)
	}
}

func defaultPolicyDto() ports.PasswordPolicyDto {
	d := tenant.DefaultPasswordPolicy()
	return ports.PasswordPolicyDto{
		MinLength: d.MinLength(), RequireUppercase: d.RequireUppercase(), RequireLowercase: d.RequireLowercase(),
		RequireDigit: d.RequireDigit(), RequireSymbol: d.RequireSymbol(),
		MaxFailedAttempts: d.MaxFailedAttempts(), LockoutMinutes: d.LockoutMinutes(),
	}
}

func (f e2eFixture) getTenantDto(t *testing.T, tok, path string) ports.TenantDto {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, path, tok, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, r.status, r.body)
	}
	var dto ports.TenantDto
	if err := json.Unmarshal(r.body, &dto); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return dto
}

func TestE2E_FreshTenant_EveryReadReportsTheDefaultPasswordPolicy(t *testing.T) {
	f := newE2EFixture(t)
	fresh := f.registerAndLogin(t, "defpol")
	opTok := f.mintPlatformToken(t, "")
	want := defaultPolicyDto()

	for _, c := range []struct{ name, tok, path string }{
		{"detail", fresh.AccessToken, "/api/v1/tenants/" + fresh.TenantID},
		{"by-slug", fresh.AccessToken, "/api/v1/tenants/by-slug/" + fresh.Slug},
		{"operator detail", opTok, "/api/v1/tenants/" + fresh.TenantID},
		{"operator by-slug", opTok, "/api/v1/tenants/by-slug/" + fresh.Slug},
	} {
		dto := f.getTenantDto(t, c.tok, c.path)
		requireInContractPolicy(t, c.name, dto.PasswordPolicy)
		if diff := cmp.Diff(want, dto.PasswordPolicy); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.name, diff)
		}
	}

	// Tenant list (?slug= filter) and the operator's list of every tenant.
	for _, path := range []string{"/api/v1/tenants?slug=" + fresh.Slug, "/api/v1/platform/tenants"} {
		r := f.authedJSON(t, http.MethodGet, path, opTok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, r.status, r.body)
		}
		var lr ports.ListTenantsResponse
		if err := json.Unmarshal(r.body, &lr); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		found := false
		for _, tn := range lr.Tenants {
			requireInContractPolicy(t, path+" "+tn.Slug, tn.PasswordPolicy)
			if tn.ID == fresh.TenantID {
				found = true
				if diff := cmp.Diff(want, tn.PasswordPolicy); diff != "" {
					t.Errorf("%s (-want +got):\n%s", path, diff)
				}
			}
		}
		if !found {
			t.Errorf("%s: the fresh tenant is not listed: %s", path, r.body)
		}
	}

	// Capabilities: the rules a member sees.
	r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", fresh.AccessToken, nil)
	if r.status != http.StatusOK {
		t.Fatalf("capabilities: %d %s", r.status, r.body)
	}
	var caps ports.CapabilitiesDto
	if err := json.Unmarshal(r.body, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if p := caps.PasswordPolicy; p.MinLength != want.MinLength || !p.RequiresUppercase || !p.RequiresLowercase || !p.RequiresDigit || !p.RequiresSymbol {
		t.Errorf("capabilities password_policy = %+v, want the default", p)
	}
}

// What a client that read the projected default sends back is accepted, persists,
// and reads back identical.
func TestE2E_FreshTenant_SecuritySettingsSaveRoundTrips(t *testing.T) {
	f := newE2EFixture(t)
	fresh := f.registerAndLogin(t, "defrt")
	path := "/api/v1/tenants/" + fresh.TenantID

	before := f.getTenantDto(t, fresh.AccessToken, path)
	r := f.authedJSON(t, http.MethodPatch, path+"/settings", fresh.AccessToken,
		ports.UpdateTenantSettingsRequest{PasswordPolicy: before.PasswordPolicy})
	if r.status != http.StatusNoContent {
		t.Fatalf("saving the read-back default: %d %s", r.status, r.body)
	}
	after := f.getTenantDto(t, fresh.AccessToken, path)
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("tenant after saving its own default (-before +after):\n%s", diff)
	}
	requireInContractPolicy(t, "after save", after.PasswordPolicy)
}

// The same class of defect, for the other fresh-resource shapes: every array the
// spec marks required must be an array ([]), never null, on a company that has
// just been created and has nothing in it yet; and the capabilities' rank / tier
// stay inside their declared range / enum.
func TestE2E_FreshTenant_RequiredArraysAreNeverNull(t *testing.T) {
	f := newE2EFixture(t)
	fresh := f.registerAndLogin(t, "defarr")
	opTok := f.mintPlatformToken(t, "")
	for _, c := range []struct{ path, tok, key string }{
		{"/api/v1/auth/me/capabilities", fresh.AccessToken, "permissions"},
		{"/api/v1/auth/me/capabilities", fresh.AccessToken, "effective_permissions"},
		{"/api/v1/auth/me/capabilities", fresh.AccessToken, "roles"},
		{"/api/v1/roles", fresh.AccessToken, "roles"},
		{"/api/v1/users", fresh.AccessToken, "users"},
		{"/api/v1/auth/sessions", fresh.AccessToken, "sessions"},
		{"/api/v1/permission-requests", fresh.AccessToken, "requests"},
		{"/api/v1/tenants?slug=no-such-" + fresh.Slug, opTok, "tenants"},
		{"/api/v1/platform/impersonation/sessions", opTok, "sessions"},
	} {
		r := f.authedJSON(t, http.MethodGet, c.path, c.tok, nil)
		if r.status != http.StatusOK {
			t.Errorf("GET %s: %d %s", c.path, r.status, r.body)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r.body, &m); err != nil {
			t.Errorf("decode %s: %v", c.path, err)
			continue
		}
		if raw := string(m[c.key]); raw == "" || raw == "null" {
			t.Errorf("GET %s: required array %q is %q, want []", c.path, c.key, raw)
		}
	}

	r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/capabilities", fresh.AccessToken, nil)
	var caps ports.CapabilitiesDto
	if err := json.Unmarshal(r.body, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if caps.Rank < 0 || caps.Rank > 99 || caps.Tier == "" {
		t.Errorf("capabilities rank/tier = %d/%q is outside the declared range", caps.Rank, caps.Tier)
	}
}
