//go:build integration

// TestMain + shared pool for the companysettings/adapters integration package.
// One Postgres testcontainer; per-test isolation via a fresh tenant row + RLS.

package adapters_test

import (
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
	identityadapters "github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// nolint:gochecknoglobals // canonical fixedNow pinned-time pattern
var fixedNow = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := pgtest.RunMain(m, pgtest.Config{
		TruncateSchemas: []string{"identity", "companysettings"},
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

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}

// seedTenant inserts a tenant row (the FK target of every companysettings table).
func seedTenant(t *testing.T, p *pgxpool.Pool) tenant.ID {
	t.Helper()
	repo := identityadapters.NewTenantRepository(p, pg.NewTransactor(p))
	id := tenant.ID(ids.NewV7().String())
	full := ids.NewV7().String()
	s, err := slug.New("cs-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@cs.test")
	tn, err := tenant.New(id, s, "Cs Pharma Pvt Ltd", "Cs", addr, fixedNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return id
}
