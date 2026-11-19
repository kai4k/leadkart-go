//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// notifications/adapters integration package. One Postgres testcontainer per
// package; isolation via fresh tenant_id + RLS per test.

package adapters_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgtest"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	identityadapters "github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var fixedNow = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		TruncateSchemas: []string{"identity", "notifications"},
	}, func(c *pgtest.Container) {
		sharedPG = c
	})
	if err := goleak.Find(pgtest.GoleakOptions()...); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func repoFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// seedTenant inserts a tenant row via identity.TenantRepository, satisfying the
// FK on notifications.notifications.tenant_id.
func seedTenant(t *testing.T, pool *pgxpool.Pool) tenant.ID {
	t.Helper()
	tx := pg.NewTransactor(pool)
	repo := identityadapters.NewTenantRepository(pool, tx)
	id := tenant.ID(ids.NewV7().String())
	full := ids.NewV7().String()
	s, err := slug.New("ntf-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@ntf.test")
	tn, err := tenant.New(id, s, "Ntf Pharma Pvt Ltd", "Ntf", addr, fixedNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return id
}

func tenantCtx(t *testing.T, id tenant.ID) context.Context {
	t.Helper()
	return tenancy.WithID(t.Context(), tenancy.ID(id.String()))
}
