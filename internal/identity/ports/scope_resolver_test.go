package ports

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Review G2b L4: an operator's X-Tenant-Id write is allowed into an ACTIVE company only. A
// suspended company is refused, and so is a pending one (no route produces pending today, but
// the next self-signup flow will); an unknown id is unknown.
func TestTenantScopeResolver_OnlyAnActiveCompanyIsActive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	repo := tenanttest.NewFakeRepository()
	add := func(slugText, id string, prepare func(*tenant.Tenant)) tenant.ID {
		t.Helper()
		sl, err := slug.New(slugText)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := email.New("admin@" + slugText + ".test")
		if err != nil {
			t.Fatal(err)
		}
		tn, err := tenant.New(tenant.ID(id), sl, slugText+" Pvt Ltd", slugText, admin, now)
		if err != nil {
			t.Fatal(err)
		}
		prepare(tn)
		if err := repo.Add(t.Context(), tn); err != nil {
			t.Fatal(err)
		}
		return tn.ID()
	}
	pending := add("pending-co", "11111111-1111-1111-1111-111111111111", func(*tenant.Tenant) {})
	active := add("active-co", "22222222-2222-2222-2222-222222222222", func(tn *tenant.Tenant) { _ = tn.Activate(now) }) // arch-test:ignore-err - fixture setup
	suspended := add("suspended-co", "33333333-3333-3333-3333-333333333333", func(tn *tenant.Tenant) {
		_ = tn.Activate(now)                   // arch-test:ignore-err - fixture setup
		_ = tn.Suspend("billing overdue", now) // arch-test:ignore-err - fixture setup
	})

	r := NewTenantScopeResolver(app.Application{Queries: app.Queries{GetTenant: query.NewGetTenantHandler(repo)}})
	for _, tc := range []struct {
		name string
		id   string
		want authn.TenantScopeState
	}{
		{"active", active.String(), authn.TenantScopeActive},
		{"suspended", suspended.String(), authn.TenantScopeInactive},
		{"pending", pending.String(), authn.TenantScopeInactive},
		{"unknown", "99999999-9999-9999-9999-999999999999", authn.TenantScopeUnknown},
	} {
		got, err := r.Lookup(t.Context(), tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: state %v, want %v", tc.name, got, tc.want)
		}
	}
}
