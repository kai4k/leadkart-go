//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Cross-tenant tenant lookups (by slug, by ?slug= filter) are an operator
// authority, not platform-tenant membership: a platform member without
// platform.tenants.view gets the same enumeration-safe answer as any outsider.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_TenantLookup_CrossTenantNeedsOperatorAuthority(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "lookupb")
	p := permission.IdentityPermissions
	zero := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)
	viewer := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)
	super := f.mintPlatformTokenWith(t, nil, true)

	bySlug := "/api/v1/tenants/by-slug/" + b.Slug
	if r := f.authedJSON(t, http.MethodGet, bySlug, zero, nil); r.status != http.StatusNotFound {
		t.Errorf("zero-permission platform member by-slug: %d %s, want 404 (enumeration-safe)", r.status, r.body)
	}
	for name, tok := range map[string]string{"platform.tenants.view": viewer, "super user": super} {
		if r := f.authedJSON(t, http.MethodGet, bySlug, tok, nil); r.status != http.StatusOK {
			t.Errorf("%s by-slug: %d %s, want 200", name, r.status, r.body)
		}
	}

	listed := func(tok string) int {
		r := f.authedJSON(t, http.MethodGet, "/api/v1/tenants?slug="+b.Slug, tok, nil)
		var lr ports.ListTenantsResponse
		if err := json.Unmarshal(r.body, &lr); err != nil || r.status != http.StatusOK {
			t.Fatalf("list by slug: %d %s", r.status, r.body)
		}
		return len(lr.Tenants)
	}
	if n := listed(zero); n != 0 {
		t.Errorf("zero-permission platform member ?slug=: %d tenants, want 0", n)
	}
	if n := listed(viewer); n != 1 {
		t.Errorf("platform.tenants.view ?slug=: %d tenants, want 1", n)
	}
	// An ordinary tenant admin still reads its own tenant.
	if r := f.authedJSON(t, http.MethodGet, bySlug, b.AccessToken, nil); r.status != http.StatusOK {
		t.Errorf("tenant admin own slug: %d %s", r.status, r.body)
	}
}
