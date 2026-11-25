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
)

// G2p: the platform tenant's id is the id of the tenant with slug "platform", read from the store;
// absent until that tenant exists, then cached.
func TestPlatformTenantResolver_ReadsTheTenantWithThePlatformSlug(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repo := tenanttest.NewFakeRepository()
	add := func(slugText, id string) {
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
		if err := repo.Add(t.Context(), tn); err != nil {
			t.Fatal(err)
		}
	}
	resolve := NewPlatformTenantResolver(app.Application{Queries: app.Queries{GetTenantBySlug: query.NewGetTenantBySlugHandler(repo)}})

	add("acme", "11111111-1111-1111-1111-111111111111")
	if got := resolve(t.Context()); got != "" {
		t.Fatalf("no platform tenant yet: got %q, want empty", got)
	}
	add("platform", "22222222-2222-2222-2222-222222222222")
	if got := resolve(t.Context()); got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("platform tenant id = %q", got)
	}
}
