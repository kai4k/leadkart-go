package ports_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Both slug lookups decide with ONE permission decision in the resolved tenant
// (authn.SatisfiesInTenant): identity.tenants.view in the caller's own tenant,
// or the platform.tenants.view equivalent for an operator. Ids compare as UUID
// values, never as raw strings.
func TestSlugLookups_DecideViaSatisfiesInTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	repo := tenanttest.NewFakeRepository()
	s, err := slug.New("acme")
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@example.test") // arch-test:ignore-err - fixed literal
	tn, err := tenant.New(tenant.ID(azTenant), s, "Acme Pharma Pvt Ltd", "Acme", addr, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var a app.Application
	a.Queries.GetTenantBySlug = query.NewGetTenantBySlugHandler(repo)

	operator := func(perms ...string) *jwt.Claims {
		c := azClaims(perms...)
		c.TenantID, c.TenantSlug, c.IsPlatform = azOther, authn.PlatformTenantSlug, true
		return c
	}
	upper := azClaims(p.Tenants.View)
	upper.TenantID = strings.ToUpper(azTenant)
	foreign := azClaims(p.Tenants.View)
	foreign.TenantID = azOther

	for _, tc := range []struct {
		name    string
		claims  *jwt.Claims
		visible bool
	}{
		{"own tenant with tenants.view", azClaims(p.Tenants.View), true},
		{"own tenant, upper-case claim spelling", upper, true},
		{"tenant.admin of own tenant", azClaims(p.Meta.TenantAdmin), true},
		{"operator with platform.tenants.view", operator(p.Platform.TenantsView), true},
		{"other tenant's member", foreign, false},
		{"operator without platform.tenants.view", operator(p.Tenants.View), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := azServeApp(t, a, tc.claims, "GET", "/api/v1/tenants?slug=acme", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list by slug: %d %s", rec.Code, rec.Body.String())
			}
			var list struct {
				Tenants []json.RawMessage `json:"tenants"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if (len(list.Tenants) == 1) != tc.visible {
				t.Errorf("list: %d tenants, visible want %v", len(list.Tenants), tc.visible)
			}
			rec = azServeApp(t, a, tc.claims, "GET", "/api/v1/tenants/by-slug/acme", "")
			want := http.StatusOK
			if !tc.visible {
				want = http.StatusNotFound
			}
			if rec.Code != want {
				t.Errorf("by-slug: %d %s, want %d", rec.Code, rec.Body.String(), want)
			}
		})
	}
}
