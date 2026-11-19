//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Tenant lookups (by slug, by ?slug= filter) read a tenant's full profile (GST,
// PAN, drug licence, admin phone and address, password policy), so they need
// identity.tenants.view (tenant.admin implies it in its own tenant) or
// platform.tenants.view. Authentication alone is not enough. Inside the gate the
// answers stay enumeration-safe: an ordinary tenant sees only its own tenant.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) listBySlug(t *testing.T, tok, slug string) (int, int) {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants?slug="+slug, tok, nil)
	if r.status != http.StatusOK {
		return r.status, 0
	}
	var lr ports.ListTenantsResponse
	if err := json.Unmarshal(r.body, &lr); err != nil {
		t.Fatalf("list by slug: %d %s", r.status, r.body)
	}
	return r.status, len(lr.Tenants)
}

func TestE2E_TenantLookup_CrossTenantNeedsOperatorAuthority(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "lookupb")
	p := permission.IdentityPermissions
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	viewer := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)
	super := f.mintPlatformTokenWith(t, nil, true)

	bySlug := "/api/v1/tenants/by-slug/" + b.Slug
	// A platform member without any view permission is refused outright.
	if r := f.authedJSON(t, http.MethodGet, bySlug, zero, nil); r.status != http.StatusForbidden {
		t.Errorf("zero-permission platform member by-slug: %d %s, want 403", r.status, r.body)
	}
	if code, _ := f.listBySlug(t, zero, b.Slug); code != http.StatusForbidden {
		t.Errorf("zero-permission platform member ?slug=: %d, want 403", code)
	}
	for name, tok := range map[string]string{"platform.tenants.view": viewer, "super user": super} {
		if r := f.authedJSON(t, http.MethodGet, bySlug, tok, nil); r.status != http.StatusOK {
			t.Errorf("%s by-slug: %d %s, want 200", name, r.status, r.body)
		}
	}
	if code, n := f.listBySlug(t, viewer, b.Slug); code != http.StatusOK || n != 1 {
		t.Errorf("platform.tenants.view ?slug=: %d with %d tenants, want 200 with 1", code, n)
	}
	// An ordinary tenant admin still reads its own tenant.
	if r := f.authedJSON(t, http.MethodGet, bySlug, b.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("tenant admin own slug: %d %s", r.status, r.body)
	}
}

// A member of an ordinary tenant with no permissions used to read the whole
// TenantDto of their own tenant through the two lookups (authentication-only);
// tenants/{id} already refused them. Now all three agree.
func TestE2E_TenantLookup_ZeroPermissionMemberRefused(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "lookupown")
	_, memberTok := f.createMemberAndLogin(t, owner, "lookupmember")

	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+owner.TenantID, memberTok, nil); r.status != http.StatusForbidden {
		t.Fatalf("control, GET /tenants/{id}: %d %s, want 403", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/by-slug/"+owner.Slug, memberTok, nil); r.status != http.StatusForbidden {
		t.Errorf("zero-permission member by-slug (own tenant): %d %s, want 403", r.status, r.body)
	}
	if code, _ := f.listBySlug(t, memberTok, owner.Slug); code != http.StatusForbidden {
		t.Errorf("zero-permission member ?slug= (own tenant): %d, want 403", code)
	}
}

// A tenant admin sees its own tenant and nothing else, with the enumeration-safe
// answers (404 / empty list) for any other slug.
func TestE2E_TenantLookup_TenantAdminSeesOnlyOwnTenant(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "lookupa")
	b := f.registerAndLogin(t, "lookupb2")

	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/by-slug/"+a.Slug, a.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("admin by-slug own: %d %s, want 200", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/by-slug/"+b.Slug, a.AccessToken, nil); r.status != http.StatusNotFound {
		t.Errorf("admin by-slug other tenant: %d %s, want 404", r.status, r.body)
	}
	if code, n := f.listBySlug(t, a.AccessToken, a.Slug); code != http.StatusOK || n != 1 {
		t.Errorf("admin ?slug= own: %d with %d, want 200 with 1", code, n)
	}
	if code, n := f.listBySlug(t, a.AccessToken, b.Slug); code != http.StatusOK || n != 0 {
		t.Errorf("admin ?slug= other tenant: %d with %d, want 200 with 0", code, n)
	}
}
