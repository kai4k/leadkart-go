//go:build integration

// SQL-CONTRACT COVERAGE for this file (ADR 0062): the platform-scope keyset
// listing of tenants with a stored policy, against real Postgres.
//
// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` bound execution.
//
// arch-test:parallel-safe — tenants are minted per test and the assertions only
// concern the tenants the test created.

package adapters_test

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
)

func TestStoredTenantsLister_ListsStoredVersionsOnlyInKeysetOrderAcrossTenants(t *testing.T) {
	t.Parallel()
	p := pool(t)
	repo := newRepo(t)
	lister := adapters.NewStoredTenantsLister(p, pg.NewTransactor(p))

	stored1, stored2, noRow, zeroRow := seedTenant(t, p), seedTenant(t, p), seedTenant(t, p), seedTenant(t, p)
	seed(t, repo, stored1)
	seed(t, repo, stored2)
	// A version-0 row (created to lock a tenant that changed nothing) is not a stored policy.
	if err := repo.UpdateByID(t.Context(), zeroRow, func(*commercesettings.Settings) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}

	// Walk the whole table in pages of one: the platform scope sees every tenant.
	var walked []uuid.UUID
	after := uuid.Nil
	for {
		page, err := lister.ListStoredTenants(t.Context(), after, 1)
		if err != nil {
			t.Fatalf("ListStoredTenants: %v", err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		after = page[len(page)-1]
	}
	if !slices.IsSortedFunc(walked, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) }) {
		t.Fatalf("not in keyset order: %v", walked)
	}
	has := func(id string) bool { return slices.Contains(walked, uuid.MustParse(id)) }
	if !has(stored1.String()) || !has(stored2.String()) {
		t.Fatalf("a tenant with a stored policy is missing from %v", walked)
	}
	if has(noRow.String()) || has(zeroRow.String()) {
		t.Fatalf("a tenant with no stored policy (or only a version-0 row) was listed: %v", walked)
	}
}
