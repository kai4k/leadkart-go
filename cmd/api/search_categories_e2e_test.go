//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// GET /api/v1/search is gated per category: the route needs ANY of
// platform.tenants.view / platform.users.view, each category needs its own
// permission, and an explicitly requested category the caller may not see is a
// 403 naming it.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_Search_GatedPerCategory(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	f.registerAndLogin(t, "srchcat")

	tenantsOnly := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)
	usersOnly := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.UsersView}, false)
	both := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView}, false)
	none := mintOperatorJWTExact(t, f.Issuer, f.persons, "", nil, false)

	cases := []struct {
		name        string
		token       string
		include     string
		wantStatus  int
		wantTenants bool
		wantPersons bool
		wantNamed   string // category the 403 must name
	}{
		{"tenants-only, default", tenantsOnly, "", http.StatusOK, true, false, ""},
		{"tenants-only, include=tenants", tenantsOnly, "&include=tenants", http.StatusOK, true, false, ""},
		{"tenants-only, include=persons", tenantsOnly, "&include=persons", http.StatusForbidden, false, false, "persons"},
		{"tenants-only, include=persons,tenants", tenantsOnly, "&include=persons,tenants", http.StatusForbidden, false, false, "persons"},
		{"users-only, default", usersOnly, "", http.StatusOK, false, true, ""},
		{"users-only, include=persons", usersOnly, "&include=persons", http.StatusOK, false, true, ""},
		{"users-only, include=tenants", usersOnly, "&include=tenants", http.StatusForbidden, false, false, "tenants"},
		{"both, default", both, "", http.StatusOK, true, true, ""},
		{"both, include=persons,tenants", both, "&include=persons,tenants", http.StatusOK, true, true, ""},
		{"neither", none, "", http.StatusForbidden, false, false, ""},
	}
	for _, tc := range cases {
		r := f.authedJSON(t, http.MethodGet, "/api/v1/search?q=srchcat"+tc.include, tc.token, nil)
		if r.status != tc.wantStatus {
			t.Errorf("%s: %d %s, want %d", tc.name, r.status, r.body, tc.wantStatus)
			continue
		}
		if tc.wantNamed != "" && !strings.Contains(string(r.body), tc.wantNamed) {
			t.Errorf("%s: 403 body must name %q: %s", tc.name, tc.wantNamed, r.body)
		}
		if r.status != http.StatusOK {
			continue
		}
		var sr ports.SearchResponse
		if err := json.Unmarshal(r.body, &sr); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if (len(sr.Tenants) > 0) != tc.wantTenants || (len(sr.Persons) > 0) != tc.wantPersons {
			t.Errorf("%s: tenants=%d persons=%d, want tenants=%v persons=%v", tc.name, len(sr.Tenants), len(sr.Persons), tc.wantTenants, tc.wantPersons)
		}
	}
}

// A cached answer for one authorised category set is never served to a caller
// with a different one.
func TestE2E_Search_CacheDoesNotCrossCategories(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	f.registerAndLogin(t, "srchcache")
	both := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView}, false)
	tenantsOnly := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)

	if r := f.authedJSON(t, http.MethodGet, "/api/v1/search?q=srchcache", both, nil); r.status != http.StatusOK {
		t.Fatalf("warm cache: %d %s", r.status, r.body)
	}
	r := f.authedJSON(t, http.MethodGet, "/api/v1/search?q=srchcache", tenantsOnly, nil)
	var sr ports.SearchResponse
	if err := json.Unmarshal(r.body, &sr); err != nil || r.status != http.StatusOK {
		t.Fatalf("tenants-only after a both-categories request: %d %s", r.status, r.body)
	}
	if len(sr.Persons) != 0 {
		t.Errorf("persons leaked from a cache entry made for a wider caller: %d hits", len(sr.Persons))
	}
}

// A tenant in the terminal `deleted` state is not searchable. The query used to
// exclude a status ('hard_deleted') that does not exist, so deleted tenants kept
// showing up in operator search.
func TestE2E_Search_DoesNotReturnDeletedTenants(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)

	live := bareTenant(t, f, "srchlive")
	gone := bareTenant(t, f, "srchgone")
	repo := adapters.NewTenantRepository(f.Pool, pg.NewTransactor(f.Pool))
	if err := repo.UpdateByID(t.Context(), gone, func(tn *tenant.Tenant) (bool, error) {
		return true, tn.HardDelete(testNow)
	}); err != nil {
		t.Fatalf("transition to deleted: %v", err)
	}

	r := f.authedJSON(t, http.MethodGet, "/api/v1/search?q=srch&include=tenants", op, nil)
	if r.status != http.StatusOK {
		t.Fatalf("search: %d %s", r.status, r.body)
	}
	var sr ports.SearchResponse
	if err := json.Unmarshal(r.body, &sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := map[string]bool{}
	for _, h := range sr.Tenants {
		seen[h.ID] = true
	}
	if !seen[string(live)] {
		t.Errorf("control: the live tenant %s is missing from the results", live)
	}
	if seen[string(gone)] {
		t.Errorf("the deleted tenant %s is returned by search", gone)
	}
}
